package glib_test

import (
	"os"
	"testing"

	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/core/intern/leaktest"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestMain(m *testing.M) {
	gtk.Init()
	os.Exit(m.Run())
}

// TestSignalLifecycleRefCount verifies that connecting and disconnecting a
// signal doesn't permanently inflate the reference count.
// The signal lifecycle must not leave an extra native reference behind.
func TestSignalLifecycleRefCount(t *testing.T) {
	p := leaktest.NewGObject()
	obj := glib.Take(p)
	tracked := leaktest.Track(p)

	refBefore := tracked.RefCount()

	id := obj.Connect("notify", func() {})
	obj.HandlerDisconnect(id)

	refAfter := tracked.RefCount()
	if refAfter != refBefore {
		t.Errorf("LEAK: ref_count after disconnect is %d, expected %d "+
			"(signal lifecycle retained an extra reference)", refAfter, refBefore)
	}
}

// TestGeneratedClosureRefCount verifies ConnectGeneratedClosure.
// NotifyProperty emits "notify::<prop>"; use "name"
// (a real property on GObject) to avoid a vacuously passing test.
func TestGeneratedClosureRefCount(t *testing.T) {
	p := leaktest.NewGObject()
	obj := glib.Take(p)
	tracked := leaktest.Track(p)

	refBefore := tracked.RefCount()

	id := obj.NotifyProperty("name", func() {})
	obj.HandlerDisconnect(id)

	refAfter := tracked.RefCount()
	if refAfter != refBefore {
		t.Errorf("LEAK: ref_count after NotifyProperty+Disconnect is %d, expected %d",
			refAfter, refBefore)
	}
}

// TestSignalStillFires verifies that signal delivery remains functional.
// Use gtk.Button with obj.Emit("clicked")
// (a no-param signal). Emit("notify") would warn without a GParamSpec.
func TestSignalStillFires(t *testing.T) {
	btn := gtk.NewButton()

	var called int
	btn.ConnectClicked(func() {
		called++
	})

	glib.BaseObject(btn).Emit("clicked")

	if called != 1 {
		t.Errorf("signal handler not called: got %d, want 1", called)
	}
}
