//go:build windows

// Package capture implements the screen-capture back ends used to *verify*
// that the overlay is excluded from recording.
//
// Each back end grabs pixels the same way a real recorder would, so the
// verifier can assert the overlay's marker colour is absent.
package capture

import (
	"errors"
	"fmt"
	"unsafe"

	"ooi/internal/winapi"
)

// Image is a captured BGRA frame, top-down.
type Image struct {
	Pix  []byte
	W, H int
}

// At returns the B,G,R bytes at (x, y).
func (im Image) At(x, y int) (b, g, r byte, ok bool) {
	if x < 0 || y < 0 || x >= im.W || y >= im.H {
		return 0, 0, 0, false
	}
	i := (y*im.W + x) * 4
	if i+3 >= len(im.Pix) {
		return 0, 0, 0, false
	}
	return im.Pix[i], im.Pix[i+1], im.Pix[i+2], true
}

// CountNear returns how many pixels are within tol of the given BGR colour.
func (im Image) CountNear(b, g, r byte, tol int) int {
	n := 0
	for i := 0; i+3 < len(im.Pix); i += 4 {
		if absDiff(im.Pix[i], b) <= tol &&
			absDiff(im.Pix[i+1], g) <= tol &&
			absDiff(im.Pix[i+2], r) <= tol {
			n++
		}
	}
	return n
}

func absDiff(a, b byte) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// dibReader wraps a memory DC + DIB used as a capture target.
type dibReader struct {
	dc   uintptr
	bmp  uintptr
	old  uintptr
	bits unsafe.Pointer
	w, h int
}

func newDIBReader(w, h int) (*dibReader, error) {
	screen := winapi.GetDC(0)
	if screen == 0 {
		return nil, errors.New("capture: GetDC(0) failed")
	}
	defer winapi.ReleaseDC(0, screen)

	dc := winapi.CreateCompatibleDC(screen)
	if dc == 0 {
		return nil, errors.New("capture: CreateCompatibleDC failed")
	}
	bi := winapi.BITMAPINFO{
		Header: winapi.BITMAPINFOHEADER{
			BiSize:        uint32(unsafe.Sizeof(winapi.BITMAPINFOHEADER{})),
			BiWidth:       int32(w),
			BiHeight:      -int32(h),
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: winapi.BI_RGB,
		},
	}
	var bits unsafe.Pointer
	bmp, err := winapi.CreateDIBSection(dc, &bi, winapi.DIB_RGB_COLORS, &bits, 0, 0)
	if err != nil || bits == nil {
		winapi.DeleteDC(dc)
		return nil, fmt.Errorf("capture: CreateDIBSection: %w", err)
	}
	old := winapi.SelectObject(dc, bmp)
	return &dibReader{dc: dc, bmp: bmp, old: old, bits: bits, w: w, h: h}, nil
}

func (d *dibReader) image() Image {
	src := unsafe.Slice((*byte)(d.bits), d.w*d.h*4)
	out := make([]byte, len(src))
	copy(out, src)
	return Image{Pix: out, W: d.w, H: d.h}
}

func (d *dibReader) close() {
	if d.old != 0 {
		winapi.SelectObject(d.dc, d.old)
	}
	if d.bmp != 0 {
		winapi.DeleteObject(d.bmp)
	}
	if d.dc != 0 {
		winapi.DeleteDC(d.dc)
	}
}

// GDIScreen captures a screen rectangle with BitBlt against the screen DC.
// This is the classic capture path used by countless recorders and by
// PrintScreen-style tools.
func GDIScreen(x, y, w, h int) (Image, error) {
	if w <= 0 || h <= 0 {
		return Image{}, errors.New("capture: empty rectangle")
	}
	screen := winapi.GetDC(0)
	if screen == 0 {
		return Image{}, errors.New("capture: GetDC(0) failed")
	}
	defer winapi.ReleaseDC(0, screen)

	d, err := newDIBReader(w, h)
	if err != nil {
		return Image{}, err
	}
	defer d.close()

	// CAPTUREBLT is what makes BitBlt include layered windows; without it a
	// pass would be meaningless because layered windows are skipped anyway.
	if !winapi.BitBlt(d.dc, 0, 0, int32(w), int32(h), screen, int32(x), int32(y),
		winapi.SRCCOPY|winapi.CAPTUREBLT) {
		return Image{}, errors.New("capture: BitBlt failed")
	}
	return d.image(), nil
}

// PrintWindowOf captures a specific window with PrintWindow, the API used to
// grab a single window's contents even when it is occluded.
func PrintWindowOf(hwnd uintptr) (Image, error) {
	r, ok := winapi.GetWindowRect(hwnd)
	if !ok {
		return Image{}, errors.New("capture: GetWindowRect failed")
	}
	w, h := int(r.Width()), int(r.Height())
	if w <= 0 || h <= 0 {
		return Image{}, errors.New("capture: window has empty rectangle")
	}
	d, err := newDIBReader(w, h)
	if err != nil {
		return Image{}, err
	}
	defer d.close()

	if !winapi.PrintWindow(hwnd, d.dc, winapi.PW_RENDERFULLCONTENT) {
		// A hard failure here is itself evidence the window resists capture,
		// but report it rather than silently passing.
		return Image{}, errors.New("capture: PrintWindow failed")
	}
	return d.image(), nil
}

// ScreenBounds returns the virtual screen origin and size.
func ScreenBounds() (x, y, w, h int) {
	const (
		smXVirtualScreen  = 76
		smYVirtualScreen  = 77
		smCXVirtualScreen = 78
		smCYVirtualScreen = 79
	)
	return int(winapi.GetSystemMetrics(smXVirtualScreen)),
		int(winapi.GetSystemMetrics(smYVirtualScreen)),
		int(winapi.GetSystemMetrics(smCXVirtualScreen)),
		int(winapi.GetSystemMetrics(smCYVirtualScreen))
}
