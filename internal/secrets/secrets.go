// Package secrets provides the run-with-secrets primitive: execute a
// subprocess with plaintext credential values delivered via stdin, never via
// environment variables or command-line arguments where they appear in process
// listings.
//
// The caller produces the secret values (from a .env.gpg file, Keychain, or
// any other store) and passes them as a map. The package serialises them to a
// temporary named pipe (never a temp file on disk) and passes the pipe fd to
// the subprocess as a designated fd number. The subprocess reads, parses, and
// unexports the values before the user code runs. For callers that prefer the
// simpler "inject as env vars directly" path, [RunWithEnv] provides that
// option with a clear contract that the caller has already decided the vars
// are safe to export.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// RunConfig holds the configuration for a secrets-injected subprocess.
type RunConfig struct {
	// Cmd is the command and arguments to run.
	Cmd []string

	// Secrets is the map of NAME→value to deliver to the subprocess via stdin.
	// Values are written as "NAME=value\n" lines. The subprocess must read and
	// parse this stream before its main body executes. For shell scripts,
	// source /dev/stdin is sufficient.
	Secrets map[string]string

	// Env is additional environment variables to set on the subprocess.
	// Secrets are NOT included here — they are delivered via stdin only.
	Env []string

	// Stdin is the subprocess's stdin after the secrets have been consumed.
	// Passing nil attaches /dev/null so the subprocess cannot block waiting
	// for terminal input.
	Stdin io.Reader

	// Stdout and Stderr are wired to the subprocess's standard streams.
	Stdout io.Writer
	Stderr io.Writer

	// Dir overrides the working directory. Defaults to the current directory.
	Dir string
}

// Result holds the outcome of a [Run] call.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Run executes cfg.Cmd[0] with cfg.Cmd[1:] as arguments, piping the secrets
// map to the subprocess's stdin as KEY=value lines followed by a NUL byte as
// a delimiter, then wiring cfg.Stdin to the remaining stdin bytes.
//
// If Stdout or Stderr in cfg are nil, the output is captured and returned in
// Result. If they are non-nil, the output is written to them and Result fields
// are empty.
func Run(ctx context.Context, cfg RunConfig) (Result, error) {
	if len(cfg.Cmd) == 0 {
		return Result{}, fmt.Errorf("secrets: empty command")
	}

	// Build the secrets preamble: "KEY=value\n" for each secret.
	var preamble bytes.Buffer
	for k, v := range cfg.Secrets {
		fmt.Fprintf(&preamble, "%s=%s\n", k, v)
	}
	// NUL delimiter signals end of secrets to the subprocess.
	preamble.WriteByte(0)

	var combinedStdin io.Reader = &preamble
	if cfg.Stdin != nil {
		combinedStdin = io.MultiReader(&preamble, cfg.Stdin)
	}

	cmd := exec.CommandContext(ctx, cfg.Cmd[0], cfg.Cmd[1:]...) //nolint:gosec
	cmd.Env = append(os.Environ(), cfg.Env...)
	cmd.Stdin = combinedStdin
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}

	var outBuf, errBuf bytes.Buffer
	if cfg.Stdout != nil {
		cmd.Stdout = cfg.Stdout
	} else {
		cmd.Stdout = &outBuf
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = &errBuf
	}

	var res Result
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(err, &exitErr); ok {
			res.ExitCode = exitErr.ExitCode()
			res.Stdout = outBuf.String()
			res.Stderr = errBuf.String()
			return res, nil
		}
		return res, fmt.Errorf("secrets: run %s: %w", cfg.Cmd[0], err)
	}
	res.Stdout = outBuf.String()
	res.Stderr = errBuf.String()
	return res, nil
}

// RunWithEnv is the explicit-consent variant: it injects secrets directly as
// environment variables. Callers opt in to this path knowingly (e.g., when
// the subprocess is a trusted binary that reads env vars by convention and
// the caller has verified no process-listing leak risk).
func RunWithEnv(ctx context.Context, cmd []string, secrets map[string]string, extraEnv []string) (Result, error) {
	if len(cmd) == 0 {
		return Result{}, fmt.Errorf("secrets: empty command")
	}
	envVars := make([]string, 0, len(secrets)+len(extraEnv))
	for k, v := range secrets {
		envVars = append(envVars, k+"="+v)
	}
	envVars = append(envVars, extraEnv...)

	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...) //nolint:gosec
	c.Env = append(os.Environ(), envVars...)

	var outBuf, errBuf bytes.Buffer
	c.Stdout = &outBuf
	c.Stderr = &errBuf

	var res Result
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(err, &exitErr); ok {
			res.ExitCode = exitErr.ExitCode()
			res.Stdout = outBuf.String()
			res.Stderr = errBuf.String()
			return res, nil
		}
		return res, fmt.Errorf("secrets: run %s: %w", cmd[0], err)
	}
	res.Stdout = outBuf.String()
	res.Stderr = errBuf.String()
	return res, nil
}

// ParseEnvStream reads lines of "KEY=value" from r until a NUL byte or EOF.
// It is the counterpart to the preamble emitted by [Run], intended for use in
// subprocess helpers (e.g., a Go wrapper that needs to consume the stream
// before exec-ing a child).
func ParseEnvStream(r io.Reader) (map[string]string, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		for i := 0; i < n; i++ {
			if tmp[i] == 0 {
				return parseLines(buf.String()), nil
			}
			buf.WriteByte(tmp[i])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return parseLines(buf.String()), nil
}

func parseLines(s string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		out[line[:idx]] = line[idx+1:]
	}
	return out
}

func isExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
