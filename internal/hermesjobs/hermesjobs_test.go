package hermesjobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/hermesjobs"
)

func fakeRunner(stdout string, exit int, err error) hermesjobs.Runner {
	return func(_ context.Context, args ...string) (string, int, error) {
		return stdout, exit, err
	}
}

func TestListJSONArray(t *testing.T) {
	m := &hermesjobs.Manager{Run: fakeRunner(`[{"name":"j1","schedule":"* * * * *","command":"echo","enabled":true}]`, 0, nil)}
	jobs, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "j1" {
		t.Errorf("got %v", jobs)
	}
}

func TestListJSONL(t *testing.T) {
	m := &hermesjobs.Manager{Run: fakeRunner(`{"name":"a","schedule":"*","command":"x"}
{"name":"b","schedule":"*","command":"y"}`, 0, nil)}
	jobs, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
}

func TestInstallIdempotent(t *testing.T) {
	calls := 0
	m := &hermesjobs.Manager{
		Run: func(_ context.Context, args ...string) (string, int, error) {
			calls++
			if args[0] == "cronjob" && args[1] == "list" {
				return `[{"name":"x","schedule":"*","command":"y"}]`, 0, nil
			}
			return "", 0, nil
		},
	}
	created, err := m.Install(context.Background(), hermesjobs.Job{Name: "x", Schedule: "*", Command: "y"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if created {
		t.Error("Install should be no-op for identical existing job")
	}
}

func TestInstallReplacesDifferent(t *testing.T) {
	var removed bool
	m := &hermesjobs.Manager{
		Run: func(_ context.Context, args ...string) (string, int, error) {
			if args[0] == "cronjob" && args[1] == "list" {
				return `[{"name":"x","schedule":"*","command":"OLD"}]`, 0, nil
			}
			if args[0] == "cronjob" && args[1] == "remove" {
				removed = true
				return "", 0, nil
			}
			return "", 0, nil
		},
	}
	created, err := m.Install(context.Background(), hermesjobs.Job{Name: "x", Schedule: "*", Command: "NEW"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !created {
		t.Error("should have re-installed differing job")
	}
	if !removed {
		t.Error("should have called remove before re-install")
	}
}

func TestRemoveIdempotent(t *testing.T) {
	m := &hermesjobs.Manager{Run: fakeRunner("not found", 1, nil)}
	if err := m.Remove(context.Background(), "missing"); err != nil {
		t.Errorf("Remove of missing job should not error: %v", err)
	}
}

func TestStatusNotFound(t *testing.T) {
	m := &hermesjobs.Manager{Run: fakeRunner(`[]`, 0, nil)}
	_, err := m.Status(context.Background(), "missing")
	if !errors.Is(err, hermesjobs.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListEmpty(t *testing.T) {
	m := &hermesjobs.Manager{Run: fakeRunner(strings.TrimSpace(""), 0, nil)}
	jobs, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d", len(jobs))
	}
}
