//go:build windows

package capture

import (
	"testing"
	"time"
)

func TestImageAt(t *testing.T) {
	im := Image{
		W: 2, H: 2,
		Pix: []byte{
			1, 2, 3, 255, 4, 5, 6, 255,
			7, 8, 9, 255, 10, 11, 12, 255,
		},
	}
	b, g, r, ok := im.At(1, 1)
	if !ok || b != 10 || g != 11 || r != 12 {
		t.Errorf("At(1,1) = %d,%d,%d ok=%v, want 10,11,12 true", b, g, r, ok)
	}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {2, 0}, {0, 2}} {
		if _, _, _, ok := im.At(p[0], p[1]); ok {
			t.Errorf("At(%d,%d) reported ok for an out-of-bounds pixel", p[0], p[1])
		}
	}
}

func TestCountNear(t *testing.T) {
	im := Image{
		W: 2, H: 2,
		Pix: []byte{
			100, 100, 100, 255, // exact
			105, 100, 100, 255, // within tolerance 5
			120, 100, 100, 255, // outside tolerance 5
			0, 0, 0, 255, // nowhere near
		},
	}
	if got := im.CountNear(100, 100, 100, 0); got != 1 {
		t.Errorf("CountNear(tol=0) = %d, want 1", got)
	}
	if got := im.CountNear(100, 100, 100, 5); got != 2 {
		t.Errorf("CountNear(tol=5) = %d, want 2", got)
	}
	if got := im.CountNear(100, 100, 100, 25); got != 3 {
		t.Errorf("CountNear(tol=25) = %d, want 3", got)
	}
}

func TestScreenBounds(t *testing.T) {
	x, y, w, h := ScreenBounds()
	if w <= 0 || h <= 0 {
		t.Fatalf("ScreenBounds returned %dx%d at (%d,%d), want a positive size", w, h, x, y)
	}
	if w > 65536 || h > 65536 {
		t.Errorf("implausible virtual screen size %dx%d", w, h)
	}
}

func TestGDIScreenCaptures(t *testing.T) {
	const w, h = 64, 48
	img, err := GDIScreen(0, 0, w, h)
	if err != nil {
		t.Fatalf("GDIScreen: %v", err)
	}
	if img.W != w || img.H != h {
		t.Errorf("captured %dx%d, want %dx%d", img.W, img.H, w, h)
	}
	if len(img.Pix) != w*h*4 {
		t.Errorf("pixel buffer is %d bytes, want %d", len(img.Pix), w*h*4)
	}
}

func TestGDIScreenRejectsEmptyRect(t *testing.T) {
	if _, err := GDIScreen(0, 0, 0, 10); err == nil {
		t.Error("GDIScreen accepted a zero width")
	}
	if _, err := GDIScreen(0, 0, 10, -1); err == nil {
		t.Error("GDIScreen accepted a negative height")
	}
}

func TestPrintWindowOfInvalidHandle(t *testing.T) {
	if _, err := PrintWindowOf(0); err == nil {
		t.Error("PrintWindowOf(0) succeeded, want an error")
	}
}

// TestDXGIDuplication exercises the Desktop Duplication path. It is skipped
// rather than failed when the API is unavailable, which happens on headless
// sessions, in some VMs, and while a secure desktop is up.
func TestDXGIDuplication(t *testing.T) {
	d, err := NewDuplicator(0, 0)
	if err != nil {
		t.Skipf("Desktop Duplication unavailable here: %v", err)
	}
	defer d.Close()

	if d.W <= 0 || d.H <= 0 {
		t.Fatalf("duplicator reports a %dx%d output", d.W, d.H)
	}
	if d.Bounds.Right <= d.Bounds.Left || d.Bounds.Bottom <= d.Bounds.Top {
		t.Errorf("implausible output bounds %+v", d.Bounds)
	}

	img, err := d.Grab(5 * time.Second)
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if img.W != d.W || img.H != d.H {
		t.Errorf("grabbed %dx%d, want %dx%d", img.W, img.H, d.W, d.H)
	}
	if len(img.Pix) != img.W*img.H*4 {
		t.Fatalf("pixel buffer is %d bytes, want %d", len(img.Pix), img.W*img.H*4)
	}

	// A real desktop is not a single flat colour; an all-identical buffer
	// would mean the copy silently failed.
	first := img.Pix[0:4]
	varied := false
	for i := 4; i+3 < len(img.Pix); i += 4096 {
		if img.Pix[i] != first[0] || img.Pix[i+1] != first[1] || img.Pix[i+2] != first[2] {
			varied = true
			break
		}
	}
	if !varied {
		t.Error("captured frame is a single flat colour, the copy probably failed")
	}

	// A second grab must work off the same session.
	if _, err := d.Grab(5 * time.Second); err != nil {
		t.Errorf("second Grab: %v", err)
	}
}

func TestDXGIScreenWrapper(t *testing.T) {
	img, bounds, err := DXGIScreen(0, 0, 5*time.Second)
	if err != nil {
		t.Skipf("Desktop Duplication unavailable here: %v", err)
	}
	if img.W <= 0 || img.H <= 0 {
		t.Errorf("DXGIScreen returned a %dx%d image", img.W, img.H)
	}
	if bounds.Right-bounds.Left != int32(img.W) {
		t.Errorf("bounds width %d does not match image width %d",
			bounds.Right-bounds.Left, img.W)
	}
}
