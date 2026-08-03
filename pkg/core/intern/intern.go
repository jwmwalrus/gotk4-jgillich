// Package intern implements value interning for Cgo sharing.
package intern

// #cgo pkg-config: gobject-2.0
// #include "intern.h"
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"

	"github.com/diamondburned/gotk4/pkg/core/closure"
	"github.com/diamondburned/gotk4/pkg/core/gdebug"

	_ "go4.org/unsafe/assume-no-moving-gc"
)

var (
	traceObjects  = gdebug.HasKey("trace-objects")
	toggleRefs    = gdebug.HasKey("toggle-refs")
	objectProfile *pprof.Profile
)

func init() {
	if gdebug.HasKey("profile-objects") {
		objectProfile = pprof.NewProfile("gotk4-object-box")
	}
}

func objInfo(obj unsafe.Pointer) slog.Attr {
	return slog.Group(
		"gobject",
		slog.String("ptr", fmt.Sprintf("%p", obj)),
		slog.String("type", C.GoString(C.gotk4_object_type_name(C.gpointer(obj)))),
		slog.Int("refs", objRefCount(obj)))
}

func objRefCount(obj unsafe.Pointer) int {
	return int(C.g_atomic_int_get((*C.gint)(unsafe.Pointer(&(*C.GObject)(obj).ref_count))))
}

// Box is an opaque type holding extra data.
//
// The finalizer is set on boxDummy (not on Box itself) because the Go GC
// does not call finalizers on objects that are members of a reference
// cycle. boxDummy is deliberately kept outside the closure cycle (it is
// only reachable from Box.dummy) so that the GC will always collect it
// once the cycle containing Box is swept.
type Box struct {
	dummy          *boxDummy
	closures       atomic.Pointer[closure.Registry]
	gobject        unsafe.Pointer
	finalizing     atomic.Bool // CAS guard: exactly one of {finalizeBox, Free} wins
	finalizerArmed bool        // protected by shared.mu
}

// boxDummy holds the finalizer. It stores only the C object pointer
// (unsafe.Pointer to C memory — the GC does not trace C addresses) and
// is NOT part of any Go reference cycle.
//
// baseRef records whether the box owns a plain g_object_ref beyond the
// toggle ref. This is true only for Take() of a non-floating object,
// where the transferred ref is kept alongside the toggle ref (the
// floating-handling path in Get consumes the transferred ref instead).
// When the toggle ref is removed, an owned base ref must also be dropped
// or the C object never reaches ref_count 0. baseRef lives on boxDummy
// (not Box) because finalizeBox may run after Box has been swept — dummy
// is always alive while the finalizer runs.
type boxDummy struct {
	gobject unsafe.Pointer
	baseRef bool
}

// Object returns Box's C GObject pointer.
func (b *Box) GObject() unsafe.Pointer { return b.gobject }

// Closures returns the closure registry for this Box.
func (b *Box) Closures() *closure.Registry {
	closures := b.closures.Load()
	if closures != nil {
		return closures
	}

	closures = closure.NewRegistry()
	if !b.closures.CompareAndSwap(nil, closures) {
		return b.closures.Load()
	}

	return closures
}

// shared contains shared closure data.
var shared = struct {
	mu sync.RWMutex
	// weak stores weak.Pointer[Box] while the object's Go reference
	// is the only remaining reference. The weak.Pointer is invisible
	// to the GC, so the Box can be collected when the closure cycle
	// becomes unreachable.
	weak map[unsafe.Pointer]weak.Pointer[Box]
	// strong stores *Box while the object is still referenced by C.
	strong map[unsafe.Pointer]*Box
	// cleanup stores the dummy for an object whose toggle reference is being
	// removed. The map entry remains until g_object_remove_toggle_ref has
	// returned so a concurrent toggle notification cannot race the map state.
	cleanup map[unsafe.Pointer]*boxDummy
}{
	weak:    make(map[unsafe.Pointer]weak.Pointer[Box], 1024),
	strong:  make(map[unsafe.Pointer]*Box, 1024),
	cleanup: make(map[unsafe.Pointer]*boxDummy, 1024),
}

// newBox creates a new Box for the given GObject and registers a finalizer
// on boxDummy (not on Box itself). boxDummy is kept outside the closure
// cycle so the GC can always collect it once the cycle is swept.
//
// baseRef reports whether the box owns a plain ref (see boxDummy.baseRef).
func newBox(obj unsafe.Pointer, baseRef bool) *Box {
	box := &Box{}
	box.gobject = obj

	box.dummy = &boxDummy{gobject: obj, baseRef: baseRef}
	runtime.SetFinalizer(box.dummy, finalizeBox)
	box.finalizerArmed = true

	if objectProfile != nil {
		objectProfile.Add(obj, 3)
	}

	if traceObjects {
		slog.Debug(
			"allocating new box for object",
			"stack", string(debug.Stack()),
			objInfo(obj))
	}

	return box
}

// TryGet gets the Box associated with the GObject or nil if it's gone.
func TryGet(gobject unsafe.Pointer) *Box {
	shared.mu.RLock()
	box, _ := gets(gobject)
	shared.mu.RUnlock()
	return box
}

// Get gets the interned box for the given GObject C pointer.
func Get(gobject unsafe.Pointer, take bool) *Box {
	if gobject == nil {
		return nil
	}

	shared.mu.RLock()
	if _, pending := shared.cleanup[gobject]; pending {
		shared.mu.RUnlock()
		return nil
	}
	box, _ := gets(gobject)
	shared.mu.RUnlock()
	if box != nil {
		return box
	}

	shared.mu.Lock()
	if _, pending := shared.cleanup[gobject]; pending {
		shared.mu.Unlock()
		return nil
	}

	box, _ = gets(gobject)
	if box != nil {
		shared.mu.Unlock()
		return box
	}

	box = newBox(gobject, take && C.g_object_is_floating(C.gpointer(gobject)) == C.FALSE)
	shared.strong[gobject] = box

	if toggleRefs {
		slog.Debug(
			"Get: will introduce new box for object",
			objInfo(gobject))
	}

	shared.mu.Unlock()

	C.g_object_add_toggle_ref(
		(*C.GObject)(gobject),
		(*[0]byte)(C.goToggleNotify), nil,
	)

	if toggleRefs {
		slog.Debug(
			"Get: added toggle reference to object",
			objInfo(gobject))
	}

	if C.g_object_is_floating(C.gpointer(gobject)) != C.FALSE {
		C.g_object_ref_sink(C.gpointer(gobject))
		C.g_object_unref(C.gpointer(gobject))

		if toggleRefs {
			slog.Debug(
				"Get: ref_sink'd the object",
				objInfo(gobject))
		}
	}

	if !take {
		C.g_object_unref(C.gpointer(gobject))
		if toggleRefs {
			slog.Debug(
				"Get: not taking, so unref'd the object",
				objInfo(gobject))
		}
	}

	return box
}

// Free explicitly frees the box permanently. It must not be resurrected
// after this.
//
// Mutual exclusion with finalizeBox: both functions claim cleanup by
// installing shared.cleanup[obj] under shared.mu. The entry remains until
// the toggle reference has actually been removed, so a notification racing
// that operation cannot observe a half-cleaned registry.
//
// Free is synchronous because it is an explicit operation and is expected to
// be called by the owner on its normal GLib thread. GC finalizers use the
// default main context instead; a finalizer must not run GObject cleanup on
// the runtime finalizer goroutine.
func Free(box *Box) {
	if box == nil {
		return
	}

	// CAS guard against double-Free on the same Box pointer.
	if !box.finalizing.CompareAndSwap(false, true) {
		return
	}

	obj := box.gobject
	if obj == nil {
		// The object has already been freed.
		return
	}

	shared.mu.Lock()
	if _, pending := shared.cleanup[obj]; pending {
		shared.mu.Unlock()
		return
	}
	if _, weakPresent := shared.weak[obj]; !weakPresent {
		if _, strongPresent := shared.strong[obj]; !strongPresent {
			shared.mu.Unlock()
			return
		}
	}

	// Keep the intern entry until the C removal completes. This is important
	// for callbacks fired by GLib during destruction and also gives the
	// finalizer a single, observable cleanup state.
	shared.cleanup[obj] = box.dummy
	if box.finalizerArmed {
		runtime.SetFinalizer(box.dummy, nil)
		box.finalizerArmed = false
	}
	shared.mu.Unlock()
	disconnectClosures(box, obj)

	// Capture diagnostic data BEFORE the potentially-freeing call.
	var info slog.Attr
	if toggleRefs {
		info = objInfo(obj)
	}

	C.g_object_remove_toggle_ref(
		(*C.GObject)(obj),
		(*[0]byte)(C.goToggleNotify), nil,
	)
	finishCleanup(obj)

	// Do NOT dereference obj after the remove call. Use pre-captured info.
	if toggleRefs {
		slog.Debug("Free: toggle ref removed", info)
	}
}

// finalizeBox is the finalizer for boxDummy. It fires when Box becomes
// unreachable, including via cycle collection: boxDummy is deliberately
// kept outside the closure cycle, so the GC always sweeps it when the
// cycle containing Box becomes unreachable.
//
// At this point Box may already have been swept by the GC (the finalizer
// is on boxDummy, not on Box), so we MUST NOT access Box fields without
// going through shared.mu and the weak map. We rely on dummy.gobject
// (a C pointer the GC does not trace) and on the shared maps.
//
// Mutual exclusion with intern.Free: both use shared.cleanup as the claim
// token. See Free for a full description. Claiming happens under a single
// shared.mu critical section, so exactly one path performs the toggle-ref
// removal.
//
// Cleanup is queued on the default main context because finalizers run on an
// arbitrary runtime goroutine. goFinishRemovingToggleRef completes the
// bookkeeping after the C call returns.
//
// The finalizer is not re-armed while cleanup is claimed. It is re-armed only
// in the race-recovery path where C re-referenced the object between the
// finalizer being queued and this callback running.
func finalizeBox(dummy *boxDummy) {
	if dummy == nil {
		return
	}
	obj := dummy.gobject

	shared.mu.Lock()
	if _, pending := shared.cleanup[obj]; pending {
		shared.mu.Unlock()
		return
	}

	// Check if the object is still strongly referenced by C.
	// If so, re-arm the finalizer and wait for the next GC cycle.
	if box, ok := shared.strong[obj]; ok {
		var info slog.Attr
		if toggleRefs {
			info = objInfo(obj)
		}
		box.finalizerArmed = true
		runtime.SetFinalizer(dummy, finalizeBox)
		shared.mu.Unlock()

		if toggleRefs {
			slog.Debug("finalizeBox: object still strong, re-arming", info)
		}
		return
	}

	// Only proceed if the weak entry still exists. If it is gone, the object
	// was already cleaned up or never completed interning.
	_, present := shared.weak[obj]
	if !present {
		shared.mu.Unlock()
		return
	}

	// The toggle reference is still installed while the weak entry is
	// present, so it is safe to gather diagnostics here.
	var info slog.Attr
	if toggleRefs {
		info = objInfo(obj)
	}

	// We claimed it. Mark finalizing so goToggleNotify knows to skip further
	// transitions. Do not delete the closure registry here: the C closures
	// own their callback lifetime and their finalize notifiers must remain
	// able to retire those entries.
	var boxToDisconnect *Box
	if wp, ok := shared.weak[obj]; ok {
		if box := wp.Value(); box != nil {
			boxToDisconnect = box
			box.finalizing.Store(true)
			if box.finalizerArmed {
				runtime.SetFinalizer(box.dummy, nil)
				box.finalizerArmed = false
			}
		}
	}

	shared.cleanup[obj] = dummy

	shared.mu.Unlock()
	disconnectClosures(boxToDisconnect, obj)

	if toggleRefs {
		slog.Debug("finalizeBox: removing toggle ref", info)
	}

	// The C callback removes the toggle ref and calls
	// goFinishRemovingToggleRef, which drops the owned base ref and removes
	// the intern entry after the C operation has completed.
	C.g_idle_add(
		(*[0]byte)(C.gotk4_intern_remove_toggle_ref),
		C.gpointer(obj),
	)

	// Do NOT call objInfo(obj) here — obj may already be freed. The actual
	// removal and final log happen in the main-context callback.
	if toggleRefs {
		slog.Debug("finalizeBox: toggle ref removal queued", info)
	}
}

// finishCleanup completes the Go side of a toggle-ref removal. It is called
// only after g_object_remove_toggle_ref has returned, so the cleanup entry
// keeps all state available while GLib is running destruction callbacks.
func finishCleanup(gobject unsafe.Pointer) {
	shared.mu.Lock()
	dummy := shared.cleanup[gobject]
	if dummy == nil {
		shared.mu.Unlock()
		return
	}
	delete(shared.cleanup, gobject)
	delete(shared.strong, gobject)
	delete(shared.weak, gobject)
	shared.mu.Unlock()

	if dummy.baseRef {
		C.g_object_unref(C.gpointer(gobject))
	}
	if objectProfile != nil {
		objectProfile.Remove(gobject)
	}
}

func disconnectClosures(box *Box, gobject unsafe.Pointer) {
	if box == nil {
		return
	}
	if reg := box.closures.Load(); reg != nil {
		reg.RangeSignals(func(gclosure unsafe.Pointer, _ *closure.FuncStack) bool {
			C.gotk4_intern_disconnect_closure(
				C.gpointer(gobject),
				(*C.GClosure)(gclosure),
			)
			reg.Delete(gclosure)
			return true
		})
	}
}

//go:nosplit
func gets(gobject unsafe.Pointer) (b *Box, strong bool) {
	if b, ok := shared.strong[gobject]; ok {
		return b, true
	}

	if wp, ok := shared.weak[gobject]; ok {
		if box := wp.Value(); box != nil {
			return box, false
		}
		// The weak.Pointer has expired (object was finalized).
		delete(shared.weak, gobject)
	}

	return nil, false
}

// makeStrong calls runtime.SetFinalizer. Although it is called from
// goToggleNotify (a cgo callback), cgo callbacks can grow the goroutine
// stack, so it does not require nosplit. gets remains nosplit because it
// only does map lookups and a delete of an existing key.
func makeStrong(gobject unsafe.Pointer) *Box {
	box, strong := gets(gobject)
	if toggleRefs {
		slog.Debug(
			"makeStrong: obtained box",
			"strong", strong,
			"box", box != nil,
			objInfo(gobject))
	}
	if box == nil {
		return nil
	}

	if !strong {
		shared.strong[gobject] = box
		delete(shared.weak, gobject)

		// Clear the finalizer while the object is strongly referenced by C.
		// makeWeak will re-arm it when the object drops back to weak-only.
		if box.finalizerArmed {
			runtime.SetFinalizer(box.dummy, nil)
			box.finalizerArmed = false
		}
	}

	return box
}

// makeWeak calls runtime.SetFinalizer during the strong-to-weak transition,
// so it can grow the goroutine stack when called from a cgo callback.
func makeWeak(gobject unsafe.Pointer) *Box {
	box, strong := gets(gobject)
	if toggleRefs {
		slog.Debug(
			"makeWeak: obtained box",
			"strong", strong,
			"box", box != nil,
			objInfo(gobject))
	}
	if box == nil {
		return nil
	}

	if strong {
		shared.weak[gobject] = weak.Make(box)
		delete(shared.strong, gobject)

		// Re-arm the finalizer when the object drops back to weak-only.
		// The strong-to-weak transition must re-arm the finalizer so the GC
		// can clean up objects that lose all non-toggle references again.
		//
		// Skip re-arming if a finalizer (or Free) is in progress —
		// finalizeBox itself re-arms under shared.mu in the race-recovery
		// path; double-arming would queue a redundant finalizer run.
		if !box.finalizing.Load() && !box.finalizerArmed {
			runtime.SetFinalizer(box.dummy, finalizeBox)
			box.finalizerArmed = true
		}
	}

	return box
}
