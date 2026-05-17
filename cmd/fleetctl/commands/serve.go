// serve.go implements `fleetctl serve` — boots the operator-plane
// JSON-RPC server that owera-cloud (the customer plane) dials via a
// private Cloudflare tunnel.
//
// Default bind is 127.0.0.1:9091. The port is NOT auth-gated at this
// layer — production fronts it with the Cloudflare tunnel and any
// upstream policy. Binding to a loopback default ensures a
// misconfigured deploy never exposes the surface to the public
// internet by accident.
package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/ledger"
	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/rpc"
)

var (
	serveAddr      string
	serveLedgerDir string
	serveTimeout   time.Duration
	serveLogPath   = "~/.hermes/logs/serve.jsonl"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the operator-plane JSON-RPC server (owera-cloud entry point)",
	Long: `serve boots the operator-plane JSON-RPC 2.0 server that owera-cloud
(the customer plane) calls into over a private Cloudflare tunnel.

The default bind is 127.0.0.1:9091 — production deployments expose the
port to the tunnel only, never to the public internet. There is no
auth layer at this CLI; trust is gated by the tunnel.

Registered methods:
  fleet.SubmitJob       {tenant_id, sku, cloud_job_id, inputs} → {task_id}
  fleet.CancelTask      {task_id} → {cancelled}
  fleet.HealthSnapshot  {} → {ts, gateway, workers, sku_conformance}

The server blocks until SIGINT/SIGTERM, then drains in-flight requests
up to --timeout and exits.`,
	Example: `  fleetctl serve
  fleetctl serve --addr 127.0.0.1:9091 --ledger-dir ~/.hermes/ledger
  fleetctl serve --addr 0.0.0.0:9091   # DO NOT do this without an upstream firewall`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:9091", "bind address (loopback default; production runs behind a Cloudflare tunnel)")
	serveCmd.Flags().StringVar(&serveLedgerDir, "ledger-dir", "~/.hermes/ledger", "ledger directory (stamped onto every entry written for a submitted task)")
	serveCmd.Flags().DurationVar(&serveTimeout, "shutdown-timeout", 10*time.Second, "max time to wait for in-flight requests on shutdown")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ledgerDir, err := expandHomePath(serveLedgerDir)
	if err != nil {
		return err
	}
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		return fmt.Errorf("serve: open ledger: %w", err)
	}

	resolvedLog, err := expandHomePath(serveLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("serve: open log: %w", err)
	}
	defer logger.Close()

	srv := rpc.NewServer(serveAddr)

	// Wire SubmitJob with the built-in delegate.shell.v1 router. The
	// router's DelegateFunc is the real SSH-backed primitive, injected
	// here so the rpc package itself never imports internal/ssh.
	submit := rpc.NewSubmitJobHandler(led, ledgerDir)
	submit.RegisterSKU("delegate.shell.v1", &rpc.ShellDelegateRouter{
		Ledger:   led,
		Delegate: makeServeDelegateFunc(),
	})
	srv.Register("fleet.SubmitJob", submit.Handle)

	// fleet.CancelTask — looks up the task in runregistry and invokes
	// its cancel func. Cancellation propagates through the SubmitJob
	// orchestrator context (registered by the submit handler) and the
	// in-flight delegate/swarm primitives bound to that context.
	srv.Register("fleet.CancelTask", rpc.CancelTask)

	// fleet.HealthSnapshot — 30s-cached gateway/worker/SKU snapshot
	// consumed by owera-cloud's /readyz and status.owera.ai. The
	// snapshot reads ledger metadata read-only, so we point it at
	// the same ledger dir the submit handler writes into.
	hermesHome, err := expandHomePath("~/.hermes")
	if err != nil {
		return fmt.Errorf("serve: expand hermes home: %w", err)
	}
	health := rpc.NewHealthSnapshotHandler(rpc.DefaultDeps(
		hermesHome+"/logs",
		hermesHome+"/nodes.txt",
		ledgerDir,
	))
	srv.Register("fleet.HealthSnapshot", health.Handle)

	_ = logger.Action(log.Event{
		Node:   "gateway",
		Phase:  "serve",
		Action: "listen:" + serveAddr,
		Result: log.ResultOK,
	})
	fmt.Fprintf(cmd.OutOrStdout(), "fleetctl serve: listening on %s\n", serveAddr)

	// Run the server in a goroutine so the main goroutine can wait on
	// signal + dispatch graceful shutdown.
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Fprintf(cmd.OutOrStdout(), "fleetctl serve: received %s, shutting down\n", sig)
		_ = logger.Action(log.Event{
			Node:   "gateway",
			Phase:  "serve",
			Action: "shutdown:" + sig.String(),
			Result: log.ResultOK,
		})
		ctx, cancel := context.WithTimeout(context.Background(), serveTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("serve: shutdown: %w", err)
		}
		// Wait for ListenAndServe to actually return so the deferred
		// logger.Close happens after the http loop has stopped writing.
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// makeServeDelegateFunc returns a rpc.DelegateFunc that dials via the
// real SSH dialer and runs cmd on node. Mirrors the dispatch shape of
// `fleetctl delegate --node ... --cmd ...` so behaviour stays consistent
// regardless of submission path.
//
// Returns one ledger.Entry per invocation with phase=delegate and the
// canonical action="ssh:<target>". TenantID is stamped by the router
// before append, so it is intentionally left unset here.
func makeServeDelegateFunc() rpc.DelegateFunc {
	return func(ctx context.Context, node, cmdStr string) ([]ledger.Entry, error) {
		target, err := osh.ParseTarget(node)
		if err != nil {
			return nil, fmt.Errorf("parse target %q: %w", node, err)
		}
		dialer := newDialer(osh.WithConnectTimeout(60 * time.Second))
		client, err := dialer.Dial(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", target, err)
		}
		defer client.Close()

		start := time.Now()
		res, runErr := client.Run(ctx, cmdStr)
		dur := time.Since(start).Milliseconds()

		entry := ledger.Entry{
			Phase:      "delegate",
			Action:     "ssh:" + target.String(),
			Result:     ledger.ResultOK,
			DurationMs: dur,
		}
		if runErr != nil {
			entry.Result = ledger.ResultError
			entry.StderrTail = truncateAction(runErr.Error())
			return []ledger.Entry{entry}, runErr
		}
		if res.ExitCode != 0 {
			entry.Result = ledger.ResultError
			entry.StderrTail = truncateAction(res.Stderr)
		}
		return []ledger.Entry{entry}, nil
	}
}

func serveSkill() Skill {
	return Skill{
		Name:    "serve",
		Trigger: "/serve",
		Summary: "Run the operator-plane JSON-RPC server (owera-cloud entry point over Cloudflare tunnel).",
		Args: []SkillArg{
			{Name: "--addr HOST:PORT", Description: "Bind address (default 127.0.0.1:9091; production runs behind a Cloudflare tunnel)."},
			{Name: "--ledger-dir PATH", Description: "Ledger directory (default ~/.hermes/ledger)."},
			{Name: "--shutdown-timeout DUR", Description: "Max time to drain in-flight requests on SIGINT/SIGTERM."},
		},
		Examples: []SkillExample{
			{Description: "Default loopback bind", Command: "fleetctl serve"},
			{Description: "Custom addr + ledger", Command: "fleetctl serve --addr 127.0.0.1:9091 --ledger-dir ~/.hermes/ledger"},
		},
	}
}

func init() { registerSkill("serve", serveSkill) }
