//go:build windows

package overlay

import "ooi/internal/winapi"

// Enabling and disabling the overlay.
//
// "Disabled" means the program keeps running — the stream stays connected and
// frames keep decoding — but nothing is on the screen. Getting there and back
// must never expose a frame, so the transition is ordered with care:
//
//	disable: stop painting -> wipe the surface to all-zero and hand that to
//	         DWM -> force click-through -> cloak (DWM composes it nowhere)
//
//	enable:  confirm WDA_EXCLUDEFROMCAPTURE with the OS (re-apply if needed;
//	         if it still cannot be confirmed, stay disabled) -> paint the
//	         newest frame while still cloaked -> restore click-through ->
//	         uncloak
//
// Cloaking is used instead of SW_HIDE so the window's Raw Input subscription
// is untouched; the six-tap gesture that brings the overlay back must keep
// working while it is gone. The HWND is never destroyed or recreated, so the
// capture exclusion attached to it persists across every toggle.

// SetEnabled shows or hides the overlay. Safe from any goroutine.
func (o *Overlay) SetEnabled(on bool) {
	if hwnd := o.hwnd.Load(); hwnd != 0 {
		var wp uintptr
		if on {
			wp = 1
		}
		winapi.PostMessage(hwnd, winapi.WM_APP_ENABLE, wp, 0)
	}
}

// Enabled reports whether the overlay is meant to be on screen.
func (o *Overlay) Enabled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.visible
}

// Cloaked reads the compositor's own view of whether the window is cloaked,
// rather than trusting local state.
func (o *Overlay) Cloaked() bool {
	hwnd := o.hwnd.Load()
	if hwnd == 0 {
		return false
	}
	v, err := winapi.DwmCloaked(hwnd)
	return err == nil && v != 0
}

// FailClosedCount reports how many times the overlay refused to show, or took
// itself off screen, because capture exclusion could not be confirmed.
func (o *Overlay) FailClosedCount() uint64 { return o.failClosedN.Load() }

// setEnabledUI performs the transition. It runs on the window thread.
func (o *Overlay) setEnabledUI(on bool) {
	hwnd := o.hwnd.Load()
	if hwnd == 0 {
		return
	}

	if !on {
		o.mu.Lock()
		o.visible = false
		o.mu.Unlock()
		o.hideNow(hwnd)
		o.syncHidden(true)
		o.notifyVisibility(false)
		return
	}

	o.mu.Lock()
	protect := o.protect
	o.mu.Unlock()
	if protect && !o.ensureProtected(hwnd) {
		// Never reveal a window whose exclusion cannot be confirmed.
		o.failClosedN.Add(1)
		return
	}

	o.mu.Lock()
	o.visible = true
	clickThr := o.clickThr
	o.mu.Unlock()

	// Paint while still cloaked, so the first image DWM composes is the
	// current frame rather than an empty or stale surface.
	o.redraw()
	o.setClickThrough(hwnd, clickThr)
	winapi.DwmSetBool(hwnd, winapi.DWMWA_CLOAK, false)
	winapi.SetWindowPos(hwnd, winapi.HWND_TOPMOST, 0, 0, 0, 0,
		winapi.SWP_NOMOVE|winapi.SWP_NOSIZE|winapi.SWP_NOACTIVATE)

	o.syncHidden(false)
	o.notifyVisibility(true)
}

// hideNow wipes and cloaks the window. The order matters: the compositor's
// copy of the surface is cleared before the window is cloaked, so no frame is
// retained anywhere even while it is out of sight.
func (o *Overlay) hideNow(hwnd uintptr) {
	if o.canvas.Valid() {
		o.canvas.Clear() // all-zero is fully transparent under premultiplied alpha
		o.mu.Lock()
		pos := winapi.POINT{X: int32(o.x), Y: int32(o.y)}
		size := winapi.SIZE{CX: int32(o.w), CY: int32(o.h)}
		o.mu.Unlock()
		o.pushSurface(hwnd, pos, size, 0)
	}
	// An invisible window must never swallow a click.
	o.setClickThrough(hwnd, true)
	winapi.DwmSetBool(hwnd, winapi.DWMWA_CLOAK, true)
}

// ensureProtected confirms WDA_EXCLUDEFROMCAPTURE with the OS, re-applying it
// if something cleared it.
func (o *Overlay) ensureProtected(hwnd uintptr) bool {
	if aff, err := winapi.GetWindowDisplayAffinity(hwnd); err == nil && aff == winapi.WDA_EXCLUDEFROMCAPTURE {
		return true
	}
	winapi.SetWindowDisplayAffinity(hwnd, winapi.WDA_EXCLUDEFROMCAPTURE)
	o.affinityRestored.Add(1)
	aff, err := winapi.GetWindowDisplayAffinity(hwnd)
	return err == nil && aff == winapi.WDA_EXCLUDEFROMCAPTURE
}

// failClosed takes the overlay off screen because protection could not be
// confirmed at the moment a frame was about to be shown.
func (o *Overlay) failClosed(hwnd uintptr) {
	o.failClosedN.Add(1)
	o.mu.Lock()
	o.visible = false
	o.mu.Unlock()
	o.hideNow(hwnd)
	o.syncHidden(true)
	o.notifyVisibility(false)
}

func (o *Overlay) setClickThrough(hwnd uintptr, on bool) {
	ex := winapi.GetWindowLongPtr(hwnd, winapi.GWL_EXSTYLE)
	if on {
		ex |= winapi.WS_EX_TRANSPARENT
	} else {
		ex &^= winapi.WS_EX_TRANSPARENT
	}
	winapi.SetWindowLongPtr(hwnd, winapi.GWL_EXSTYLE, ex)
}

// syncHidden keeps the tap recogniser's thresholds matched to reality.
func (o *Overlay) syncHidden(hidden bool) {
	if o.gest != nil {
		o.gest.recog.SetHidden(hidden)
	}
}

func (o *Overlay) notifyVisibility(on bool) {
	if cb := o.cfg.OnVisibility; cb != nil {
		go cb(on) // never call out while holding the window thread
	}
}
