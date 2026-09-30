//go:build windows

package verify

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ooi/internal/capture"
	"ooi/internal/overlay"
	"ooi/internal/winapi"
)

// The requirement these tests exist for: changing transparency or position by
// touchpad must not make the overlay capturable, in any screen-recording mode.
//
// The risk is real rather than theoretical. WDA_EXCLUDEFROMCAPTURE is a
// property of a window *handle*, so any implementation that recreated the
// window to resize or re-blend it would silently lose protection. These tests
// hammer opacity and position through the same code path the gestures use,
// while every capture back end takes a shot at the window.

// startMarkerOverlay brings up a protected, marker-filled overlay.
func startMarkerOverlay(t *testing.T, rect Rect) *overlay.Overlay {
	t.Helper()
	winapi.EnablePerMonitorDPI()

	ov := overlay.New(overlay.Config{
		X: rect.X, Y: rect.Y, W: rect.W, H: rect.H,
		Opacity:      255,
		ClickThrough: true,
		Protect:      true,
		Hotkeys:      false,
		// Real touchpad input is not needed: the gesture handlers funnel
		// into SetOpacity/MoveBy, which is what is exercised here.
		Gestures:    false,
		Placeholder: "gesture capture test",
	})

	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()

	select {
	case <-ov.Ready():
	case err := <-runErr:
		t.Fatalf("overlay exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("overlay did not become ready")
	}
	t.Cleanup(func() {
		ov.Stop()
		select {
		case <-ov.Closed():
		case <-time.After(5 * time.Second):
		}
	})

	ov.SetMarker(Marker[0], Marker[1], Marker[2], "")
	time.Sleep(400 * time.Millisecond)
	return ov
}

// TestProtectionHoldsWhileOpacityRamps drives a continuous opacity ramp — the
// one-finger and two-finger hold gestures — and captures with every back end
// throughout.
func TestProtectionHoldsWhileOpacityRamps(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window and drives capture APIs; skipped in -short")
	}

	rect := Rect{X: 180, Y: 180, W: 520, H: 320}
	ov := startMarkerOverlay(t, rect)

	// Ramp opacity up and down continuously, as a held finger would.
	stop := make(chan struct{})
	var ramps atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, dir := 255, -6
		for {
			select {
			case <-stop:
				return
			default:
			}
			v += dir
			if v <= 40 {
				v, dir = 40, 6
			}
			if v >= 255 {
				v, dir = 255, -6
			}
			ov.SetOpacity(v)
			ramps.Add(1)
			time.Sleep(8 * time.Millisecond)
		}
	}()

	assertInvisibleEverywhere(t, ov, rect, "while opacity was ramping")

	close(stop)
	wg.Wait()

	if ramps.Load() < 20 {
		t.Errorf("only %d opacity changes happened; the test barely exercised the ramp",
			ramps.Load())
	}
	if !ov.Protected() {
		t.Error("overlay lost capture protection after ramping opacity")
	}
	if n := ov.AffinityRestored(); n != 0 {
		t.Errorf("the watchdog had to restore protection %d time(s) during ramping; "+
			"something in the opacity path is dropping the affinity", n)
	}
}

// TestProtectionHoldsWhileMoving drives the two-finger drag path.
func TestProtectionHoldsWhileMoving(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window and drives capture APIs; skipped in -short")
	}

	rect := Rect{X: 200, Y: 200, W: 480, H: 300}
	ov := startMarkerOverlay(t, rect)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dx := 4
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%12 == 11 {
				dx = -dx
			}
			ov.MoveBy(dx, 0)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// The window is moving, so the fixed rect is only an approximation of
	// where it is. Search the whole screen instead: any marker pixel
	// anywhere is a leak.
	assertNoMarkerOnScreen(t, ov, "while the overlay was being moved")

	close(stop)
	wg.Wait()

	if !ov.Protected() {
		t.Error("overlay lost capture protection after being moved")
	}
	if n := ov.AffinityRestored(); n != 0 {
		t.Errorf("the watchdog restored protection %d time(s) during movement", n)
	}
}

// TestDisabledOverlayLeaksNothing covers the 3-tap disable / 6-tap enable
// toggle, which is the moment a naive implementation leaks: the stream keeps
// running while the overlay is "off".
//
// It is deliberately harsher than real use. While disabled, capture exclusion
// is switched OFF and marker frames keep arriving, and every capture back end
// must still see nothing. That proves the disabled state is safe on its own
// (surface wiped, window cloaked, no repaint) rather than only because of
// WDA_EXCLUDEFROMCAPTURE. A control phase first proves each back end *can*
// see the unprotected window, so an absence afterwards means something.
func TestDisabledOverlayLeaksNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window and drives capture APIs; skipped in -short")
	}
	rect := Rect{X: 220, Y: 220, W: 460, H: 280}
	ov := startMarkerOverlay(t, rect)
	area := rect.W * rect.H

	// ---- control: enabled and UNPROTECTED, so the marker must be seen ----
	if err := <-ov.SetProtect(false); err != nil {
		t.Fatalf("cannot disable protection for the control phase: %v", err)
	}
	ov.SetMarker(Marker[0], Marker[1], Marker[2], "")
	time.Sleep(500 * time.Millisecond)

	saw := map[string]bool{}
	for _, b := range Backends() {
		img, ox, oy, err := b.grab(ov, rect)
		if err != nil {
			continue
		}
		if countInRect(img, ox, oy, rect) >= int(float64(area)*minHitRatio) {
			saw[b.name] = true
		}
	}
	if len(saw) == 0 {
		t.Fatal("no back end saw the unprotected window; the test would be vacuous")
	}
	for name := range saw {
		t.Logf("control: %s sees the unprotected window", name)
	}

	// ---- disabled, protection still OFF, frames still arriving ----------
	ov.SetEnabled(false)
	time.Sleep(300 * time.Millisecond)
	if ov.Enabled() || !ov.Cloaked() {
		t.Fatalf("disable did not take effect: enabled=%v cloaked=%v", ov.Enabled(), ov.Cloaked())
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The same path a decoded video frame takes.
			ov.SetMarker(Marker[0], Marker[1], Marker[2], "")
			time.Sleep(15 * time.Millisecond)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	for _, b := range Backends() {
		if !saw[b.name] {
			continue
		}
		img, ox, oy, err := b.grab(ov, rect)
		if err != nil {
			continue // refusing to capture is not a leak
		}
		if hits := countInRect(img, ox, oy, rect); hits > int(float64(area)*maxLeakRatio) {
			t.Errorf("%s captured a DISABLED overlay: %.1f%% of the window",
				b.name, 100*float64(hits)/float64(area))
		}
	}
	close(stop)
	wg.Wait()

	// ---- re-enable with protection back ON ------------------------------
	if err := <-ov.SetProtect(true); err != nil {
		t.Fatal(err)
	}
	ov.SetEnabled(true)
	time.Sleep(300 * time.Millisecond)
	if !ov.Enabled() || ov.Cloaked() || !ov.Protected() {
		t.Fatalf("enable failed: enabled=%v cloaked=%v protected=%v",
			ov.Enabled(), ov.Cloaked(), ov.Protected())
	}
	assertInvisibleEverywhere(t, ov, rect, "after re-enabling")
}

// TestToggleCyclesKeepProtection runs repeated disable/enable cycles while
// frames stream, as a user tapping 3 and 6 times would.
func TestToggleCyclesKeepProtection(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window; skipped in -short")
	}
	rect := Rect{X: 200, Y: 200, W: 420, H: 260}
	ov := startMarkerOverlay(t, rect)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ov.SetMarker(Marker[0], Marker[1], Marker[2], "")
			time.Sleep(20 * time.Millisecond)
		}
	}()

	for i := 0; i < 8; i++ {
		ov.SetEnabled(false)
		time.Sleep(80 * time.Millisecond)
		if !ov.Protected() || !ov.Cloaked() {
			t.Fatalf("cycle %d disabled: protected=%v cloaked=%v", i, ov.Protected(), ov.Cloaked())
		}
		ov.SetEnabled(true)
		time.Sleep(80 * time.Millisecond)
		if !ov.Protected() || ov.Cloaked() {
			t.Fatalf("cycle %d enabled: protected=%v cloaked=%v", i, ov.Protected(), ov.Cloaked())
		}
	}
	assertInvisibleEverywhere(t, ov, rect, "after repeated enable/disable cycles")
	close(stop)
	wg.Wait()

	if n := ov.AffinityRestored(); n != 0 {
		t.Errorf("protection had to be restored %d time(s) during toggling", n)
	}
	if n := ov.FailClosedCount(); n != 0 {
		t.Errorf("overlay failed closed %d time(s) during normal toggling", n)
	}
}

// TestEnableRestoresClearedAffinity: if the exclusion were cleared behind the
// overlay's back while it was disabled, enabling must put it back *before*
// showing anything.
func TestEnableRestoresClearedAffinity(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window; skipped in -short")
	}
	rect := Rect{X: 260, Y: 260, W: 360, H: 220}
	ov := startMarkerOverlay(t, rect)

	ov.SetEnabled(false)
	time.Sleep(150 * time.Millisecond)
	// Clear it directly, leaving the overlay's own intent (protect=true) intact.
	if err := winapi.SetWindowDisplayAffinity(ov.HWND(), winapi.WDA_NONE); err != nil {
		t.Fatal(err)
	}
	ov.SetEnabled(true)
	time.Sleep(200 * time.Millisecond)

	if !ov.Protected() {
		t.Fatal("overlay came back on screen without capture exclusion")
	}
	if ov.AffinityRestored() == 0 {
		t.Fatal("enable did not notice the cleared affinity")
	}
	assertInvisibleEverywhere(t, ov, rect, "after enabling over a cleared affinity")
}

// TestProtectionHoldsUnderCombinedGestureStorm runs opacity, movement and
// visibility changes at once, which is the worst case for anything that
// rebuilds the window.
func TestProtectionHoldsUnderCombinedGestureStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a window and drives capture APIs; skipped in -short")
	}

	rect := Rect{X: 240, Y: 240, W: 440, H: 260}
	ov := startMarkerOverlay(t, rect)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range []func(int){
		func(i int) { ov.SetOpacity(40 + (i*7)%215) },
		func(i int) { ov.MoveBy(((i%7)-3)*3, ((i%5)-2)*3) },
		func(i int) {
			if i%20 == 0 {
				ov.SetOpacity(0)
			}
		},
		func(i int) { ov.SetEnabled(i%2 == 0) },
	} {
		wg.Add(1)
		go func(seed int, f func(int)) {
			defer wg.Done()
			for i := seed; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				f(i)
				time.Sleep(7 * time.Millisecond)
			}
		}(i, fn)
	}

	assertNoMarkerOnScreen(t, ov, "during a combined gesture storm")

	close(stop)
	wg.Wait()

	ov.SetEnabled(true)
	ov.SetOpacity(255)
	time.Sleep(200 * time.Millisecond)

	if !ov.Protected() {
		t.Error("overlay lost capture protection during the storm")
	}
	if n := ov.AffinityRestored(); n != 0 {
		t.Errorf("the watchdog restored protection %d time(s) during the storm", n)
	}
}

// assertInvisibleEverywhere runs every capture back end against a known
// rectangle and fails if any of them sees the marker.
func assertInvisibleEverywhere(t *testing.T, ov *overlay.Overlay, rect Rect, when string) {
	t.Helper()
	area := rect.W * rect.H
	limit := int(float64(area) * maxLeakRatio)

	ran := 0
	for _, b := range Backends() {
		img, ox, oy, err := b.grab(ov, rect)
		if err != nil {
			// A back end that cannot run here proves nothing either way;
			// the dedicated verify tests cover availability.
			t.Logf("%s unavailable: %v", b.name, err)
			continue
		}
		ran++
		if hits := countInRect(img, ox, oy, rect); hits > limit {
			t.Errorf("%s captured the overlay %s: %d of %d pixels (%.1f%%)",
				b.name, when, hits, area, 100*float64(hits)/float64(area))
		}
	}
	if ran == 0 {
		t.Fatalf("no capture back end could run; the assertion %s is vacuous", when)
	}
}

// assertNoMarkerOnScreen scans whole-screen captures, used when the overlay is
// moving and its exact rectangle is not known.
func assertNoMarkerOnScreen(t *testing.T, ov *overlay.Overlay, when string) {
	t.Helper()
	x, y, w, h := capture.ScreenBounds()

	type shot struct {
		name string
		grab func() (capture.Image, error)
	}
	shots := []shot{
		{"GDI BitBlt + CAPTUREBLT", func() (capture.Image, error) {
			return capture.GDIScreen(x, y, w, h)
		}},
		{"GDI StretchBlt", func() (capture.Image, error) {
			return capture.GDIStretchBlt(x, y, w, h)
		}},
		{"GDI desktop window DC", func() (capture.Image, error) {
			return capture.GDIWindowDC(x, y, w, h)
		}},
		{"DXGI Desktop Duplication", func() (capture.Image, error) {
			img, _, err := capture.DXGIScreen(x+w/2, y+h/2, 4*time.Second)
			return img, err
		}},
		{"WGC monitor capture", func() (capture.Image, error) {
			img, _, _, err := capture.WGCScreen(x+w/2, y+h/2, 5*time.Second)
			return img, err
		}},
		{"WGC window capture", func() (capture.Image, error) {
			img, _, _, err := capture.WGCWindowCapture(ov.HWND(), 5*time.Second)
			return img, err
		}},
		{"PrintWindow (full content)", func() (capture.Image, error) {
			return capture.PrintWindowOf(ov.HWND())
		}},
	}

	ran := 0
	for _, s := range shots {
		img, err := s.grab()
		if err != nil {
			t.Logf("%s unavailable: %v", s.name, err)
			continue
		}
		ran++
		// The marker is a saturated colour nothing else on a desktop shows
		// across an area, so a handful of stray matches is tolerable but a
		// visible window is not.
		hits := img.CountNear(Marker[0], Marker[1], Marker[2], tolerance)
		if limit := 2000; hits > limit {
			t.Errorf("%s captured the overlay %s: %d marker pixels (limit %d)",
				s.name, when, hits, limit)
		}
	}
	if ran == 0 {
		t.Fatalf("no capture back end could run; the assertion %s is vacuous", when)
	}
}
