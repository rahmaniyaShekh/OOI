//go:build windows

package overlay

import (
	"image"
	"image/color"
	"testing"
	"time"

	"ooi/internal/winapi"
)

// start brings up a real overlay window and tears it down with the test.
func start(t *testing.T, cfg Config) *Overlay {
	t.Helper()
	ov := New(cfg)

	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()

	select {
	case <-ov.Ready():
	case err := <-runErr:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("overlay did not become ready within 10s")
	}

	t.Cleanup(func() {
		ov.Stop()
		select {
		case <-ov.Closed():
		case <-time.After(5 * time.Second):
			t.Error("overlay did not shut down within 5s")
		}
		if err := <-runErr; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
	return ov
}

func testConfig() Config {
	return Config{
		X: 60, Y: 60, W: 320, H: 200,
		Opacity:      255,
		ClickThrough: true,
		Protect:      true,
		Hotkeys:      false, // global hotkeys would fight a parallel test run
		Placeholder:  "unit test",
	}
}

// TestProtectionIsAppliedAtCreation is the core assertion of the whole
// project: the window carries WDA_EXCLUDEFROMCAPTURE before it is ever shown.
func TestProtectionIsAppliedAtCreation(t *testing.T) {
	ov := start(t, testConfig())

	hwnd := ov.HWND()
	if hwnd == 0 {
		t.Fatal("HWND is 0 after Ready")
	}
	aff, err := winapi.GetWindowDisplayAffinity(hwnd)
	if err != nil {
		t.Fatalf("GetWindowDisplayAffinity: %v", err)
	}
	if aff != winapi.WDA_EXCLUDEFROMCAPTURE {
		t.Errorf("display affinity = %#x, want WDA_EXCLUDEFROMCAPTURE (%#x)",
			aff, winapi.WDA_EXCLUDEFROMCAPTURE)
	}
}

func TestUnprotectedWindowHasNoAffinity(t *testing.T) {
	cfg := testConfig()
	cfg.Protect = false
	ov := start(t, cfg)

	aff, err := winapi.GetWindowDisplayAffinity(ov.HWND())
	if err != nil {
		t.Fatalf("GetWindowDisplayAffinity: %v", err)
	}
	if aff != winapi.WDA_NONE {
		t.Errorf("display affinity = %#x, want WDA_NONE", aff)
	}
}

// TestNoHotkeyCanDisableProtection is a guarantee test: `serve` must not
// expose any keystroke that turns capture exclusion off. Driving every
// registered hotkey must leave the window protected.
func TestNoHotkeyCanDisableProtection(t *testing.T) {
	cfg := testConfig()
	cfg.Hotkeys = false // do not fight the developer's real keyboard
	ov := start(t, cfg)

	// Exercise every hotkey action directly, including ones that resize the
	// window and reallocate the drawing surface.
	for id := int32(hkToggleVisible); id <= hkShrinkH; id++ {
		if id == hkQuit {
			continue // would tear the window down
		}
		ov.onHotkey(id)
	}
	time.Sleep(300 * time.Millisecond)

	if !ov.Protected() {
		t.Error("a hotkey action left the overlay unprotected")
	}
	if aff, err := winapi.GetWindowDisplayAffinity(ov.HWND()); err != nil ||
		aff != winapi.WDA_EXCLUDEFROMCAPTURE {
		t.Errorf("affinity = %#x err = %v, want WDA_EXCLUDEFROMCAPTURE", aff, err)
	}
}

// TestProtectionSurvivesResize covers the risky path: resizing frees and
// recreates the DIB, and a naive implementation might recreate the window.
func TestProtectionSurvivesResize(t *testing.T) {
	cfg := testConfig()
	cfg.Hotkeys = false
	ov := start(t, cfg)

	before := ov.HWND()
	for i := 0; i < 6; i++ {
		ov.onHotkey(hkGrowW)
		ov.onHotkey(hkGrowH)
	}
	time.Sleep(300 * time.Millisecond)

	if ov.HWND() != before {
		t.Error("resizing recreated the window; the affinity would be lost")
	}
	if !ov.Protected() {
		t.Error("overlay lost protection after resizing")
	}
}

func TestProtectedReflectsOSState(t *testing.T) {
	cfg := testConfig()
	cfg.Protect = false
	ov := start(t, cfg)

	if ov.Protected() {
		t.Error("Protected() = true for an unprotected window")
	}
	if err := <-ov.SetProtect(true); err != nil {
		t.Fatalf("SetProtect: %v", err)
	}
	if !ov.Protected() {
		t.Error("Protected() = false right after enabling protection")
	}
}

// TestAffinityWatchdogRestores checks the self-healing path: if something
// external clears the exclusion, the timer puts it back.
func TestAffinityWatchdogRestores(t *testing.T) {
	ov := start(t, testConfig())

	if got := ov.AffinityRestored(); got != 0 {
		t.Fatalf("AffinityRestored = %d before tampering, want 0", got)
	}

	// Clear it behind the overlay's back, the way another process could.
	if err := winapi.SetWindowDisplayAffinity(ov.HWND(), winapi.WDA_NONE); err != nil {
		t.Fatalf("could not clear affinity for the test: %v", err)
	}
	if ov.Protected() {
		t.Fatal("affinity did not actually clear; the test proves nothing")
	}

	// The watchdog runs on a 2s timer.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ov.Protected() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ov.Protected() {
		t.Fatal("watchdog did not restore capture protection")
	}
	if got := ov.AffinityRestored(); got == 0 {
		t.Error("AffinityRestored was not incremented")
	}
}

func TestSetProtectToggles(t *testing.T) {
	cfg := testConfig()
	cfg.Protect = false
	ov := start(t, cfg)

	if err := <-ov.SetProtect(true); err != nil {
		t.Fatalf("SetProtect(true): %v", err)
	}
	aff, _ := winapi.GetWindowDisplayAffinity(ov.HWND())
	if aff != winapi.WDA_EXCLUDEFROMCAPTURE {
		t.Fatalf("after SetProtect(true) affinity = %#x, want %#x", aff, winapi.WDA_EXCLUDEFROMCAPTURE)
	}

	if err := <-ov.SetProtect(false); err != nil {
		t.Fatalf("SetProtect(false): %v", err)
	}
	aff, _ = winapi.GetWindowDisplayAffinity(ov.HWND())
	if aff != winapi.WDA_NONE {
		t.Errorf("after SetProtect(false) affinity = %#x, want WDA_NONE", aff)
	}
}

// TestWindowStyles checks the extended styles that keep the overlay out of
// the taskbar and out of the way of the mouse.
func TestWindowStyles(t *testing.T) {
	ov := start(t, testConfig())

	ex := winapi.GetWindowLongPtr(ov.HWND(), winapi.GWL_EXSTYLE)
	for _, want := range []struct {
		bit  uintptr
		name string
	}{
		{winapi.WS_EX_LAYERED, "WS_EX_LAYERED"},
		{winapi.WS_EX_TOPMOST, "WS_EX_TOPMOST"},
		{winapi.WS_EX_TOOLWINDOW, "WS_EX_TOOLWINDOW"},
		{winapi.WS_EX_NOACTIVATE, "WS_EX_NOACTIVATE"},
		{winapi.WS_EX_TRANSPARENT, "WS_EX_TRANSPARENT"},
	} {
		if ex&want.bit == 0 {
			t.Errorf("extended style is missing %s (ex=%#x)", want.name, ex)
		}
	}
}

func TestWindowRectMatchesConfig(t *testing.T) {
	cfg := testConfig()
	ov := start(t, cfg)

	r, ok := winapi.GetWindowRect(ov.HWND())
	if !ok {
		t.Fatal("GetWindowRect failed")
	}
	if int(r.Left) != cfg.X || int(r.Top) != cfg.Y {
		t.Errorf("window at (%d,%d), want (%d,%d)", r.Left, r.Top, cfg.X, cfg.Y)
	}
	if int(r.Width()) != cfg.W || int(r.Height()) != cfg.H {
		t.Errorf("window is %dx%d, want %dx%d", r.Width(), r.Height(), cfg.W, cfg.H)
	}
}

func TestSizeReportsConfiguredDimensions(t *testing.T) {
	ov := start(t, testConfig())
	w, h := ov.Size()
	if w != 320 || h != 200 {
		t.Errorf("Size() = %dx%d, want 320x200", w, h)
	}
}

// TestFrameRendersWithoutError drives the full display path: a decoded image
// goes to the UI thread, is drawn into the DIB and presented.
func TestFrameRendersWithoutError(t *testing.T) {
	ov := start(t, testConfig())

	img := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	for i := 0; i < 20; i++ {
		ov.Frame(img)
	}
	time.Sleep(300 * time.Millisecond)

	// The window must still be alive and protected after all that.
	if aff, err := winapi.GetWindowDisplayAffinity(ov.HWND()); err != nil || aff != winapi.WDA_EXCLUDEFROMCAPTURE {
		t.Errorf("after rendering: affinity = %#x, err = %v", aff, err)
	}
}

func TestStatusAndClearFrame(t *testing.T) {
	ov := start(t, testConfig())

	ov.Status("sender connected")
	ov.Frame(image.NewRGBA(image.Rect(0, 0, 32, 32)))
	time.Sleep(150 * time.Millisecond)

	ov.ClearFrame()
	ov.Status("sender disconnected")
	time.Sleep(150 * time.Millisecond)

	if ov.HWND() == 0 {
		t.Error("window went away during status updates")
	}
}

func TestSetMarkerRepaints(t *testing.T) {
	ov := start(t, testConfig())

	ov.SetMarker(0x2B, 0x0A, 0xE8, "")
	time.Sleep(150 * time.Millisecond)
	if ov.HWND() == 0 {
		t.Fatal("window went away after SetMarker")
	}

	// A multi-line label goes through the GDI text path, which is where the
	// alpha channel has to be restored afterwards.
	ov.SetMarker(0x2B, 0x0A, 0xE8, "LINE ONE\nline two\nline three")
	time.Sleep(150 * time.Millisecond)
	if ov.HWND() == 0 {
		t.Error("window went away after a labelled SetMarker")
	}
}

// TestFrameBeforeRunIsSafe covers the race where the server starts delivering
// before the window exists.
func TestFrameBeforeRunIsSafe(t *testing.T) {
	ov := New(testConfig())
	ov.Frame(image.NewRGBA(image.Rect(0, 0, 8, 8)))
	ov.Status("early status")
	ov.ClearFrame()

	if w, h := ov.Size(); w != 320 || h != 200 {
		t.Errorf("Size() before Run = %dx%d, want 320x200", w, h)
	}
	if hwnd := ov.HWND(); hwnd != 0 {
		t.Errorf("HWND before Run = %#x, want 0", hwnd)
	}
}

func TestStopBeforeRunClosesChannel(t *testing.T) {
	ov := New(testConfig())
	ov.Stop()
	select {
	case <-ov.Closed():
	case <-time.After(2 * time.Second):
		t.Error("Stop before Run did not close the Closed channel")
	}
}

func TestSetProtectBeforeRunReturnsError(t *testing.T) {
	ov := New(testConfig())
	if err := <-ov.SetProtect(true); err == nil {
		t.Error("SetProtect before Run returned nil, want an error")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	ov := New(testConfig())
	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()
	select {
	case <-ov.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}

	ov.Stop()
	ov.Stop()
	ov.Stop()

	select {
	case <-ov.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("overlay did not close")
	}
	if err := <-runErr; err != nil {
		t.Errorf("Run returned %v", err)
	}
}

// TestSequentialOverlays proves the window class registration survives being
// used more than once in a process, which is what `verify` then `serve` does.
func TestSequentialOverlays(t *testing.T) {
	for i := 0; i < 3; i++ {
		ov := New(testConfig())
		runErr := make(chan error, 1)
		go func() { runErr <- ov.Run() }()

		select {
		case <-ov.Ready():
		case err := <-runErr:
			t.Fatalf("iteration %d: Run exited early: %v", i, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: not ready", i)
		}

		ov.Stop()
		select {
		case <-ov.Closed():
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: did not close", i)
		}
		if err := <-runErr; err != nil {
			t.Fatalf("iteration %d: Run returned %v", i, err)
		}
	}
}

func TestClampHelpers(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-5, 24}, {0, 24}, {23, 24}, {24, 24}, {200, 200}, {255, 255}, {999, 255},
	} {
		if got := clampOpacity(tc.in); got != tc.want {
			t.Errorf("clampOpacity(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want int }{
		{-100, 96}, {95, 96}, {96, 96}, {1000, 1000}, {9000, 8192},
	} {
		if got := clampDim(tc.in); got != tc.want {
			t.Errorf("clampDim(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
