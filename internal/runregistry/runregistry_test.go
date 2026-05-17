package runregistry_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/owera/owera-fleet/internal/runregistry"
)

func TestRegisterAndCancel(t *testing.T) {
	r := &runregistry.Registry{}
	ctx, cancel := context.WithCancel(context.Background())

	r.Register("task-1", cancel)
	if !r.Has("task-1") {
		t.Fatalf("expected task-1 to be registered")
	}
	if got := r.Cancel("task-1"); !got {
		t.Errorf("Cancel: got false, want true")
	}
	if ctx.Err() == nil {
		t.Errorf("expected context to be cancelled after Cancel call")
	}
	if r.Has("task-1") {
		t.Errorf("expected task-1 to be removed after Cancel")
	}
}

func TestCancelUnknownReturnsFalse(t *testing.T) {
	r := &runregistry.Registry{}
	if got := r.Cancel("does-not-exist"); got {
		t.Errorf("Cancel of unknown task: got true, want false")
	}
}

func TestUnregisterDoesNotCancel(t *testing.T) {
	r := &runregistry.Registry{}
	ctx, cancel := context.WithCancel(context.Background())

	r.Register("task-x", cancel)
	r.Unregister("task-x")
	if ctx.Err() != nil {
		t.Errorf("Unregister must not cancel the underlying context")
	}
	if got := r.Cancel("task-x"); got {
		t.Errorf("Cancel after Unregister: got true, want false")
	}
	cancel() // for the linter; explicitly release.
}

func TestRegisterEmptyTaskIDIsNoOp(t *testing.T) {
	r := &runregistry.Registry{}
	called := false
	r.Register("", func() { called = true })
	if r.Len() != 0 {
		t.Errorf("empty task ID should not register; Len=%d", r.Len())
	}
	if r.Cancel("") {
		t.Errorf("Cancel(\"\") must return false")
	}
	if called {
		t.Errorf("nothing should have been called")
	}
}

func TestRegisterNilCancelIsNoOp(t *testing.T) {
	r := &runregistry.Registry{}
	r.Register("task-y", nil)
	if r.Has("task-y") {
		t.Errorf("nil cancel func must not register")
	}
}

func TestRegisterOverwrite(t *testing.T) {
	r := &runregistry.Registry{}
	var firstCalled, secondCalled int32
	r.Register("task-z", func() { atomic.AddInt32(&firstCalled, 1) })
	r.Register("task-z", func() { atomic.AddInt32(&secondCalled, 1) })

	if !r.Cancel("task-z") {
		t.Fatalf("Cancel after overwrite: got false, want true")
	}
	if atomic.LoadInt32(&firstCalled) != 0 {
		t.Errorf("first cancel func should not have been called; got %d", firstCalled)
	}
	if atomic.LoadInt32(&secondCalled) != 1 {
		t.Errorf("second cancel func should have been called once; got %d", secondCalled)
	}
}

func TestConcurrentRegisterCancel(t *testing.T) {
	r := &runregistry.Registry{}
	const n = 200
	var cancelled int32
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, cancel := context.WithCancel(context.Background())
			id := taskName(i)
			r.Register(id, func() {
				atomic.AddInt32(&cancelled, 1)
				cancel()
			})
		}(i)
	}
	wg.Wait()

	if r.Len() != n {
		t.Errorf("registered %d, want %d", r.Len(), n)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !r.Cancel(taskName(i)) {
				t.Errorf("Cancel(%s) returned false", taskName(i))
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&cancelled); got != n {
		t.Errorf("cancelled count = %d, want %d", got, n)
	}
	if r.Len() != 0 {
		t.Errorf("registry should be empty after all cancels; Len=%d", r.Len())
	}
}

// TestPackageLevelDefault exercises the convenience wrappers around the
// process-global registry. We isolate by using a unique task ID and clean up
// after ourselves so this test stays safe under -count=N.
func TestPackageLevelDefault(t *testing.T) {
	const id = "test-pkg-level-task-9f3c"
	defer runregistry.Unregister(id)

	_, cancel := context.WithCancel(context.Background())
	runregistry.Register(id, cancel)
	if !runregistry.Has(id) {
		t.Fatalf("Has must see package-level registration")
	}
	if !runregistry.Cancel(id) {
		t.Errorf("Cancel must return true for registered task")
	}
	if runregistry.Has(id) {
		t.Errorf("task should be gone after Cancel")
	}
}

func taskName(i int) string {
	return "task-" + itoa(i)
}

// itoa is a tiny strconv.Itoa stand-in so the test stays dependency-free.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
