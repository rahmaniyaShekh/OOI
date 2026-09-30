//go:build windows

package overlay

import (
	"math"
	"time"

	"ooi/internal/gesture"
	"ooi/internal/touchpad"
	"ooi/internal/winapi"
)

// Touchpad gesture control.
//
// The watcher runs strictly in parallel with normal input. It subscribes to the
// touchpad through Raw Input, which delivers a *copy* of each report: there is
// no hook, nothing sits in the input chain, and no click, tap or scroll is ever
// delayed, blocked or swallowed. The touchpad behaves exactly as it always
// does; the overlay simply also watches it.
//
// Everything here runs on the overlay's window thread, driven by WM_INPUT.
// Opacity, position and visibility are all changed on the existing HWND, which
// is never destroyed and never recreated, so WDA_EXCLUDEFROMCAPTURE — a
// property of the window handle — cannot be dropped by any gesture.
//
//	3 quick taps  (while showing)  -> disable: off screen, still streaming
//	6 quick taps  (while hidden)   -> enable:  back on screen
//	1 finger held still            -> brighten
//	2 fingers held still           -> dim, then slide to move

const timerGesture = 2 // expires stale tap bursts

// gestureState is the overlay's gesture subsystem.
type gestureState struct {
	enabled bool

	reader *touchpad.Reader
	recog  *gesture.Recognizer

	events []gesture.Event

	// opacityF tracks opacity as a float so that many small ramp steps
	// accumulate smoothly instead of being lost to integer truncation.
	opacityF float64

	// moveAcc carries the sub-pixel remainder of a drag between frames.
	moveAccX, moveAccY float64

	disables int
	enables  int
	moves    int
}

// initGestures sets up touchpad control. A machine without a Precision
// Touchpad is not an error: the overlay simply keeps working with hotkeys.
func (o *Overlay) initGestures(hwnd uintptr) {
	if !o.cfg.Gestures {
		return
	}
	g := &gestureState{
		reader: touchpad.NewReader(),
		recog:  gesture.New(o.cfg.GestureConfig),
	}
	if err := g.reader.Register(hwnd); err != nil {
		o.gestureErr = err
		return
	}

	o.mu.Lock()
	g.opacityF = float64(o.opacity)
	g.recog.SetHidden(!o.visible)
	o.mu.Unlock()

	g.enabled = true
	o.gest = g
	winapi.SetTimer(hwnd, timerGesture, 250)
}

// shutdownGestures releases the raw input subscription.
func (o *Overlay) shutdownGestures(hwnd uintptr) {
	if o.gest == nil {
		return
	}
	winapi.KillTimer(hwnd, timerGesture)
	o.gest.reader.Unregister()
	o.gest = nil
}

// onRawInput handles WM_INPUT. The caller still passes the message on to
// DefWindowProc, so the system sees the input exactly as if we were not here.
func (o *Overlay) onRawInput(lParam uintptr) {
	g := o.gest
	if g == nil || !g.enabled {
		return
	}
	now := time.Now()
	g.reader.Handle(lParam, now, func(f touchpad.Frame) {
		cx, cy := f.Centroid()
		g.events = g.recog.Update(gesture.Frame{
			Count: f.Count(),
			X:     cx,
			Y:     cy,
			At:    f.At,
		}, g.events[:0])

		for _, ev := range g.events {
			o.applyGesture(ev)
		}
	})
}

// onGestureTimer expires stale state.
func (o *Overlay) onGestureTimer() {
	if g := o.gest; g != nil && g.enabled {
		g.recog.Expire(time.Now())
	}
}

// applyGesture turns one recognised event into a change on screen.
func (o *Overlay) applyGesture(ev gesture.Event) {
	g := o.gest
	switch ev.Kind {

	case gesture.KindDisable:
		g.disables++
		o.setEnabledUI(false)

	case gesture.KindEnable:
		g.enables++
		o.setEnabledUI(true)

	case gesture.KindOpacityDelta:
		// There is nothing to see change while disabled; only the six-tap
		// burst brings the overlay back.
		if !o.Enabled() {
			return
		}
		o.mu.Lock()
		g.opacityF += ev.Delta * 255
		if g.opacityF < minVisibleOpacity {
			g.opacityF = minVisibleOpacity
		}
		if g.opacityF > 255 {
			g.opacityF = 255
		}
		o.opacity = int(g.opacityF + 0.5)
		o.mu.Unlock()
		o.present()

	case gesture.KindMove:
		if !o.Enabled() {
			return
		}
		// Accumulate the fractional part rather than truncating each frame.
		//
		// A touchpad reports at ~100Hz, so a slow, careful drag produces a
		// long run of sub-pixel deltas. Truncating every one of them to an
		// int would round each to zero and the overlay would refuse to move
		// at all below a certain finger speed — precisely when the user is
		// trying to position it accurately.
		g.moveAccX += ev.DX
		g.moveAccY += ev.DY
		stepX := math.Trunc(g.moveAccX)
		stepY := math.Trunc(g.moveAccY)
		g.moveAccX -= stepX
		g.moveAccY -= stepY
		if stepX == 0 && stepY == 0 {
			return
		}
		g.moves++
		o.mu.Lock()
		o.x += int(stepX)
		o.y += int(stepY)
		o.clampToVirtualScreenLocked()
		o.mu.Unlock()
		o.present()
	}
}

// minVisibleOpacity keeps a ramp from dimming the overlay to the point where
// it cannot be found again. Going fully off screen is reachable only through
// the deliberate three-tap disable.
const minVisibleOpacity = 16

// clampToVirtualScreenLocked keeps the overlay reachable after a drag.
// The caller holds o.mu.
func (o *Overlay) clampToVirtualScreenLocked() {
	const (
		smXVirtualScreen  = 76
		smYVirtualScreen  = 77
		smCXVirtualScreen = 78
		smCYVirtualScreen = 79
		// Always leave this much of the overlay on screen.
		margin = 48
	)
	vx := int(winapi.GetSystemMetrics(smXVirtualScreen))
	vy := int(winapi.GetSystemMetrics(smYVirtualScreen))
	vw := int(winapi.GetSystemMetrics(smCXVirtualScreen))
	vh := int(winapi.GetSystemMetrics(smCYVirtualScreen))
	if vw <= 0 || vh <= 0 {
		return
	}
	minX, maxX := vx-o.w+margin, vx+vw-margin
	minY, maxY := vy-o.h+margin, vy+vh-margin

	if o.x < minX {
		o.x = minX
	}
	if o.x > maxX {
		o.x = maxX
	}
	if o.y < minY {
		o.y = minY
	}
	if o.y > maxY {
		o.y = maxY
	}
}

// GestureStats reports gesture activity, for `status` and for tests.
func (o *Overlay) GestureStats() (enabled bool, disables, enables, moves int, err error) {
	if o.gest == nil {
		return false, 0, 0, 0, o.gestureErr
	}
	return o.gest.enabled, o.gest.disables, o.gest.enables, o.gest.moves, o.gestureErr
}

// applyGestureForTest lets tests drive the exact code path a real touchpad
// event takes, on the window thread.
func (o *Overlay) applyGestureForTest(ev gesture.Event) { o.applyGesture(ev) }
