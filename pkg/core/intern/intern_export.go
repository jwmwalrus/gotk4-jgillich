package intern

// #cgo pkg-config: gobject-2.0
// #include "intern.h"
import "C"

import (
	"log/slog"
	"unsafe"
)

// goToggleNotify is called by GLib on each toggle notification. It doesn't
// actually free anything and relies on Box's finalizer to free both the box and
// the C GObject.
//
//export goToggleNotify
func goToggleNotify(_ C.gpointer, obj *C.GObject, isLastInt C.gboolean) {
	gobject := unsafe.Pointer(obj)
	isLast := isLastInt != C.FALSE

	shared.mu.Lock()
	defer shared.mu.Unlock()
	if _, pending := shared.cleanup[gobject]; pending {
		// The cleanup owner keeps the intern state stable until the C removal
		// returns. Do not let a re-entrant notification resurrect it.
		return
	}

	var box *Box
	if isLast {
		box = makeWeak(gobject)
	} else {
		box = makeStrong(gobject)
	}

	if box == nil {
		if toggleRefs {
			slog.Debug(
				"goToggleNotify: box not found",
				"object", unsafe.Pointer(obj))
		}
		return
	}

	if toggleRefs {
		slog.Debug(
			"goToggleNotify: finished",
			"is_last", isLast,
			"finalizing", box.finalizing.Load(),
			objInfo(unsafe.Pointer(obj)))
	}

	// If a finalizer (or Free) is in the middle of cleanup, ignore the
	// isLast transition: the toggle-ref removal path owns the lifecycle
	// now. For !isLast during finalization, makeStrong has already
	// promoted the Box back to strong and cleared the finalizer. The
	// queued finalizeBox will see either:
	//   (a) no shared.weak entry → bail (Free won the claim), OR
	//   (b) shared.strong present → re-arm and return (race recovery)
	// so makeStrong does not race a later removal.
	if box.finalizing.Load() {
		return
	}
}

// finishRemovingToggleRef is called after the toggle reference removal routine
// has returned. It removes the GObject from the global maps and drops the
// plain reference owned by the Box, if any.
//
//export goFinishRemovingToggleRef
func goFinishRemovingToggleRef(gobject unsafe.Pointer) {
	finishCleanup(gobject)
}
