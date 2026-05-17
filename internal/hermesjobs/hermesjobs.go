// Package hermesjobs is a thin wrapper around `hermes cronjob` invocations.
// It is the operator-plane primitive behind the §3.3 ticket-triage poller and
// other long-running Hermes-managed schedules — distinct from launchd-managed
// agents (which are typically system services). hermesjobs are owned by the
// Hermes runtime, share its sandbox/secrets, and survive reboots through the
// Hermes process supervisor.
//
// The package shells out to the `hermes` CLI; the actual subprocess executor
// is injected through [Runner] so tests don't depend on the local binary.
package hermesjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Job describes a Hermes-managed cron job.
type Job struct {
	Name     string            `json:"name"`
	Schedule string            `json:"schedule"`           // cron expression
	Command  string            `json:"command"`            // command Hermes should run
	Env      map[string]string `json:"env,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Enabled  bool              `json:"enabled"`
	Created  time.Time         `json:"created,omitempty"`
	LastRun  time.Time         `json:"last_run,omitempty"`
	LastExit int               `json:"last_exit,omitempty"`
}

// Runner executes the hermes CLI; tests substitute a fake.
type Runner func(ctx context.Context, args ...string) (stdout string, exitCode int, err error)

// DefaultRunner shells out to `hermes` on PATH.
func DefaultRunner(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "hermes", args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// Manager wraps a Runner with high-level operations.
type Manager struct {
	Run Runner
}

// New returns a Manager using DefaultRunner.
func New() *Manager {
	return &Manager{Run: DefaultRunner}
}

// List queries `hermes cronjob list --json` and returns the result.
func (m *Manager) List(ctx context.Context) ([]Job, error) {
	out, exit, err := m.Run(ctx, "cronjob", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("hermesjobs list: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("hermesjobs list: exit %d: %s", exit, out)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var jobs []Job
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		// Some Hermes versions emit one job per line; fall back to line scan.
		jobs = nil
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var j Job
			if err := json.Unmarshal([]byte(line), &j); err == nil {
				jobs = append(jobs, j)
			}
		}
	}
	return jobs, nil
}

// Install creates or updates a job. Idempotent: if a job with the same name
// exists with identical schedule + command, this is a no-op and ok=false.
func (m *Manager) Install(ctx context.Context, j Job) (created bool, err error) {
	existing, _ := m.List(ctx)
	for _, e := range existing {
		if e.Name == j.Name {
			if e.Schedule == j.Schedule && e.Command == j.Command {
				return false, nil
			}
			// Schedule/command differ — remove then re-install.
			if _, _, err := m.Run(ctx, "cronjob", "remove", j.Name); err != nil {
				return false, err
			}
			break
		}
	}
	args := []string{"cronjob", "install", "--name", j.Name, "--schedule", j.Schedule, "--command", j.Command}
	for k, v := range j.Env {
		args = append(args, "--env", k+"="+v)
	}
	for _, t := range j.Tags {
		args = append(args, "--tag", t)
	}
	out, exit, err := m.Run(ctx, args...)
	if err != nil {
		return false, fmt.Errorf("hermesjobs install %s: %w", j.Name, err)
	}
	if exit != 0 {
		return false, fmt.Errorf("hermesjobs install %s: exit %d: %s", j.Name, exit, out)
	}
	return true, nil
}

// Remove deletes a job by name. Idempotent: missing jobs return no error.
func (m *Manager) Remove(ctx context.Context, name string) error {
	out, exit, err := m.Run(ctx, "cronjob", "remove", name)
	if err != nil {
		return fmt.Errorf("hermesjobs remove %s: %w", name, err)
	}
	if exit != 0 && !strings.Contains(out, "not found") {
		return fmt.Errorf("hermesjobs remove %s: exit %d: %s", name, exit, out)
	}
	return nil
}

// Status returns the latest known state for a single job.
func (m *Manager) Status(ctx context.Context, name string) (*Job, error) {
	jobs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Name == name {
			return &j, nil
		}
	}
	return nil, ErrNotFound
}

// ErrNotFound is returned when a named job does not exist.
var ErrNotFound = errors.New("hermesjobs: job not found")
