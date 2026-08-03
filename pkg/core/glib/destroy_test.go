package glib_test

import (
	"testing"

	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/core/intern/leaktest"
)

// TestDestroyBreaksCycle verifies that glib.Destroy manually breaks a
// closure cycle that GC alone cannot collect, by freeing the C object
// and asserting it is actually finalized.
func TestDestroyBreaksCycle(t *testing.T) {
	p := leaktest.NewGObject()
	obj := glib.Take(p)
	tracked := leaktest.Track(p)

	// Connect a self-referencing handler — this creates a cycle
	// (Box → closures → FuncStack → this func → obj → Object → box).
	obj.NotifyProperty("name", func() {
		_ = obj
	})

	// Explicitly destroy: removes the toggle ref and drops the owned
	// base ref (baseRef), driving the object to ref_count 0.
	glib.Destroy(obj)

	leaktest.AssertFreed(t, tracked, "destroy-breaks-cycle")
}

// TestDestroyIdempotent verifies that calling Destroy twice doesn't crash
// and that the first Destroy actually freed the object.
func TestDestroyIdempotent(t *testing.T) {
	p := leaktest.NewGObject()
	obj := glib.Take(p)
	tracked := leaktest.Track(p)

	glib.Destroy(obj)
	leaktest.AssertFreed(t, tracked, "destroy-freed")

	// Repeated Destroy must be a no-op, not a crash or double-free.
	glib.Destroy(obj)
}

// TestDestroyDisconnectsSignalClosures verifies that explicit cleanup does
// not leave ordinary signal closures attached to a surviving C object.
func TestDestroyDisconnectsSignalClosures(t *testing.T) {
	p := leaktest.NewGObject()
	held := leaktest.Hold(p)
	obj := glib.Take(p)
	tracked := leaktest.Track(p)

	called := 0
	obj.NotifyProperty("name", func() { called++ })
	glib.Destroy(obj)

	leaktest.NotifyName(held)
	if called != 0 {
		t.Fatalf("destroyed signal closure was called %d times", called)
	}

	leaktest.Release(held)
	leaktest.AssertFreed(t, tracked, "destroy-disconnects-signal")
}
