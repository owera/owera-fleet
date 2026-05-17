package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/owera/owera-fleet/internal/launchd"
)

// TestSnapshotPublishTemplate_RendersExpectedFields verifies the
// snapshot-publish.plist template emits a plist with Label, the
// fleetctl binary path + "snapshot-publish" arg, KeepAlive,
// ThrottleInterval, and the standard log paths under HERMES_HOME.
func TestSnapshotPublishTemplate_RendersExpectedFields(t *testing.T) {
	m := launchd.New(nil)
	body, err := m.RenderTemplate(snapshotPublishTemplateName, launchd.Vars{
		Label:      snapshotPublishLabel,
		UID:        501,
		HomeDir:    "/Users/claw3",
		ScriptPath: "/usr/local/bin/fleetctl",
	})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`<string>com.owera.snapshot-publish</string>`,
		`<string>/usr/local/bin/fleetctl</string>`,
		`<string>snapshot-publish</string>`,
		`<key>KeepAlive</key>`,
		`<true/>`,
		`<key>ThrottleInterval</key>`,
		`<integer>10</integer>`,
		`<key>RunAtLoad</key>`,
		`<string>/Users/claw3/.hermes/logs/snapshot-publish.launchd.err</string>`,
		`<string>/Users/claw3/.hermes/logs/snapshot-publish.launchd.out</string>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plist missing %q", want)
		}
	}
}

// fakeLaunchctl captures the most-recent launchctl invocation so the
// install/uninstall handlers can be exercised without touching the
// real macOS launchd.
type fakeLaunchctl struct {
	mu      sync.Mutex
	calls   [][]string
	respond map[string][]byte
	err     map[string]error
}

func (f *fakeLaunchctl) RunCmd(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	return f.respond[key], f.err[key]
}

// newFakeManager returns a launchd.Manager that captures filesystem
// + launchctl calls in fakeLaunchctl, with a temp dir for plist
// writes.
func newFakeManager(t *testing.T) (*launchd.Manager, *fakeLaunchctl, string) {
	t.Helper()
	tmp := t.TempDir()
	fake := &fakeLaunchctl{
		respond: map[string][]byte{},
		err:     map[string]error{},
	}
	writes := map[string][]byte{}
	m := launchd.New(nil)
	m.UID = 501
	m.WriteFile = func(path string, data []byte, perm os.FileMode) error {
		writes[path] = data
		// Mirror to tmp so RemoveFile can verify the path round-trip.
		return os.WriteFile(filepath.Join(tmp, filepath.Base(path)), data, perm)
	}
	m.MkdirAll = func(_ string, _ os.FileMode) error { return nil }
	m.RemoveFile = func(path string) error {
		delete(writes, path)
		_ = os.Remove(filepath.Join(tmp, filepath.Base(path)))
		return nil
	}
	m.RunCmd = fake.RunCmd
	return m, fake, tmp
}

func TestSnapshotPublishInstall_BootstrapsViaLaunchctl(t *testing.T) {
	m, fake, _ := newFakeManager(t)
	original := newLaunchdManagerFn
	newLaunchdManagerFn = func() *launchd.Manager { return m }
	t.Cleanup(func() { newLaunchdManagerFn = original })

	cmd := snapshotPublishInstallCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(new(strings.Builder))
	if err := runSnapshotPublishInstall(cmd, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("launchctl calls: got %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got[0] != "launchctl" || got[1] != "bootstrap" {
		t.Errorf("launchctl invocation: got %v", got)
	}
	// Expect target domain gui/<UID>.
	if !strings.HasPrefix(got[2], "gui/") {
		t.Errorf("domain: got %q", got[2])
	}
	// 4th arg is the plist path.
	if !strings.Contains(got[3], snapshotPublishLabel+".plist") {
		t.Errorf("plist path: got %q", got[3])
	}
}

func TestSnapshotPublishUninstall_BootsOutAndRemoves(t *testing.T) {
	m, fake, _ := newFakeManager(t)
	original := newLaunchdManagerFn
	newLaunchdManagerFn = func() *launchd.Manager { return m }
	t.Cleanup(func() { newLaunchdManagerFn = original })

	cmd := snapshotPublishUninstallCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(new(strings.Builder))
	if err := runSnapshotPublishUninstall(cmd, nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("launchctl calls: got %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got[0] != "launchctl" || got[1] != "bootout" {
		t.Errorf("launchctl invocation: got %v", got)
	}
	if !strings.HasSuffix(got[2], "/"+snapshotPublishLabel) {
		t.Errorf("target: got %q", got[2])
	}
}
