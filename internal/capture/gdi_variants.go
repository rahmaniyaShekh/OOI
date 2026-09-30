//go:build windows

package capture

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"ooi/internal/winapi"
)

// Additional legacy capture paths.
//
// Recorders differ in exactly how they ask GDI for pixels, and the variations
// are not equivalent: BitBlt without CAPTUREBLT skips layered windows
// entirely, PrintWindow without PW_RENDERFULLCONTENT uses the legacy WM_PRINT
// path, and grabbing the desktop window's DC is a different handle from the
// screen DC. Each is checked separately so a pass means something specific.

var (
	user32Var          = syscall.NewLazyDLL("user32.dll")
	gdi32Var           = syscall.NewLazyDLL("gdi32.dll")
	procGetWindowDC    = user32Var.NewProc("GetWindowDC")
	procOpenClipboard  = user32Var.NewProc("OpenClipboard")
	procCloseClipboard = user32Var.NewProc("CloseClipboard")
	procGetClipboard   = user32Var.NewProc("GetClipboardData")
	procEmptyClipboard = user32Var.NewProc("EmptyClipboard")
	procKeybdEvent     = user32Var.NewProc("keybd_event")
	procStretchBlt     = gdi32Var.NewProc("StretchBlt")

	kernel32Var    = syscall.NewLazyDLL("kernel32.dll")
	procGlobalLock = kernel32Var.NewProc("GlobalLock")
	procGlobalUnlk = kernel32Var.NewProc("GlobalUnlock")
	procGlobalSize = kernel32Var.NewProc("GlobalSize")
)

// GDIScreenNoCaptureBlt captures with plain SRCCOPY, no CAPTUREBLT.
//
// This is what a naive recorder does. Layered windows are excluded from this
// path by GDI regardless of display affinity, so it is reported separately
// rather than being counted as evidence of protection.
func GDIScreenNoCaptureBlt(x, y, w, h int) (Image, error) {
	return gdiBlt(0, x, y, w, h, winapi.SRCCOPY)
}

// GDIWindowDC captures through GetWindowDC(GetDesktopWindow()) rather than
// the screen DC. Some capture tools use this handle instead.
func GDIWindowDC(x, y, w, h int) (Image, error) {
	desktop := winapi.GetDesktopWindow()
	src, _, _ := procGetWindowDC.Call(desktop)
	if src == 0 {
		return Image{}, errors.New("capture: GetWindowDC(desktop) failed")
	}
	defer winapi.ReleaseDC(desktop, src)
	return bltFrom(src, x, y, w, h, winapi.SRCCOPY|winapi.CAPTUREBLT)
}

// GDIStretchBlt captures via StretchBlt at 1:1, a different GDI entry point
// from BitBlt that some screen recorders use for scaling capture.
func GDIStretchBlt(x, y, w, h int) (Image, error) {
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

	r, _, _ := procStretchBlt.Call(
		d.dc, 0, 0, uintptr(w), uintptr(h),
		screen, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		uintptr(winapi.SRCCOPY|winapi.CAPTUREBLT),
	)
	if r == 0 {
		return Image{}, errors.New("capture: StretchBlt failed")
	}
	return d.image(), nil
}

// gdiBlt captures from the screen DC with the given raster operation.
func gdiBlt(_ uintptr, x, y, w, h int, rop uint32) (Image, error) {
	screen := winapi.GetDC(0)
	if screen == 0 {
		return Image{}, errors.New("capture: GetDC(0) failed")
	}
	defer winapi.ReleaseDC(0, screen)
	return bltFrom(screen, x, y, w, h, rop)
}

func bltFrom(src uintptr, x, y, w, h int, rop uint32) (Image, error) {
	if w <= 0 || h <= 0 {
		return Image{}, errors.New("capture: empty rectangle")
	}
	d, err := newDIBReader(w, h)
	if err != nil {
		return Image{}, err
	}
	defer d.close()

	if !winapi.BitBlt(d.dc, 0, 0, int32(w), int32(h), src, int32(x), int32(y), rop) {
		return Image{}, errors.New("capture: BitBlt failed")
	}
	return d.image(), nil
}

// PrintWindowLegacy captures a window without PW_RENDERFULLCONTENT, i.e. the
// original WM_PRINT-based path used before Windows 8.1.
func PrintWindowLegacy(hwnd uintptr) (Image, error) {
	return printWindowFlags(hwnd, 0)
}

// PrintWindowClientOnly captures only a window's client area.
func PrintWindowClientOnly(hwnd uintptr) (Image, error) {
	const pwClientOnly = 0x00000001
	return printWindowFlags(hwnd, pwClientOnly)
}

func printWindowFlags(hwnd uintptr, flags uint32) (Image, error) {
	r, ok := winapi.GetWindowRect(hwnd)
	if !ok {
		return Image{}, errors.New("capture: GetWindowRect failed")
	}
	w, h := int(r.Width()), int(r.Height())
	if w <= 0 || h <= 0 {
		return Image{}, errors.New("capture: window has an empty rectangle")
	}
	d, err := newDIBReader(w, h)
	if err != nil {
		return Image{}, err
	}
	defer d.close()

	if !winapi.PrintWindow(hwnd, d.dc, flags) {
		return Image{}, errors.New("capture: PrintWindow failed")
	}
	return d.image(), nil
}

// ---------------------------------------------------------- PrintScreen key

const (
	vkSnapshot       = 0x2C
	keyEventKeyUp    = 0x0002
	cfDIB            = 8
	clipboardRetries = 40

	inputKeyboard = 1
	keyEventFUp   = 0x0002
)

// keybdInput mirrors KEYBDINPUT inside INPUT. The trailing padding brings the
// struct up to the 40-byte INPUT size on amd64 (4 type + 4 pad + 32 union).
type inputRecord struct {
	Type      uint32
	_         uint32
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
	_         [8]byte
}

var procSendInput = user32Var.NewProc("SendInput")

// pressPrintScreen synthesises a Print Screen keystroke.
//
// SendInput is used rather than the deprecated keybd_event because the latter
// is unreliable for system-handled keys on modern Windows.
func pressPrintScreen() error {
	down := inputRecord{Type: inputKeyboard, Vk: vkSnapshot}
	up := inputRecord{Type: inputKeyboard, Vk: vkSnapshot, Flags: keyEventFUp}
	events := [2]inputRecord{down, up}

	n, _, err := procSendInput.Call(
		2,
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(events[0]),
	)
	runtime.KeepAlive(&events)
	if n != 2 {
		return fmt.Errorf("capture: SendInput delivered %d of 2 events: %w", n, err)
	}
	return nil
}

// fromAddr converts an address returned by a Windows API into a pointer.
//
// The address refers to memory Windows owns (the clipboard's global heap),
// never to the Go heap, so it is not subject to the garbage collector's
// pointer rules. The indirection keeps the conversion in one audited place
// instead of scattering vet suppressions through the reader.
func fromAddr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// PrintScreenClipboard presses the Print Screen key and reads the resulting
// bitmap back out of the clipboard.
//
// This is the path a user takes with the keyboard, and it is a genuinely
// separate code path inside Windows from an application calling BitBlt.
//
// It clobbers the clipboard, so it is only invoked by the verifier.
func PrintScreenClipboard(timeout time.Duration) (Image, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Clear first so a stale image cannot be mistaken for a fresh capture.
	if err := withClipboard(func() error {
		procEmptyClipboard.Call()
		return nil
	}); err != nil {
		return Image{}, err
	}

	if err := pressPrintScreen(); err != nil {
		return Image{}, err
	}

	deadline := time.Now().Add(timeout)
	for {
		img, err := readClipboardDIB()
		if err == nil {
			return img, nil
		}
		if time.Now().After(deadline) {
			// Some systems remap Print Screen (to the Snipping Tool, to a
			// vendor utility, or via a policy), and a synthesised keystroke
			// may not reach the shell at all from a background process.
			// Report that plainly rather than as a protection result.
			return Image{}, fmt.Errorf(
				"capture: Print Screen did not populate the clipboard on this system "+
					"(remapped, or the synthetic keystroke was not delivered); "+
					"test it by hand with `ooi demo`: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func withClipboard(fn func() error) error {
	var opened bool
	for i := 0; i < clipboardRetries; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !opened {
		return errors.New("capture: could not open the clipboard")
	}
	defer procCloseClipboard.Call()
	return fn()
}

// readClipboardDIB converts a CF_DIB clipboard payload into an Image.
func readClipboardDIB() (Image, error) {
	var out Image
	err := withClipboard(func() error {
		h, _, _ := procGetClipboard.Call(cfDIB)
		if h == 0 {
			return errors.New("capture: clipboard holds no CF_DIB")
		}
		// GlobalLock hands back a raw address into the clipboard's global
		// heap. It is fixed for as long as the lock is held, so reading it
		// as a byte slice is safe for the duration of this callback.
		ptr, _, _ := procGlobalLock.Call(h)
		if ptr == 0 {
			return errors.New("capture: GlobalLock failed")
		}
		defer procGlobalUnlk.Call(h)

		size, _, _ := procGlobalSize.Call(h)
		if size < unsafe.Sizeof(winapi.BITMAPINFOHEADER{}) {
			return errors.New("capture: clipboard DIB is too small")
		}
		buf := unsafe.Slice((*byte)(fromAddr(ptr)), size)

		img, err := decodeDIB(buf)
		if err != nil {
			return err
		}
		out = img
		return nil
	})
	return out, err
}

// decodeDIB converts a packed DIB (header + palette + pixels) into BGRA.
// Only the uncompressed 24- and 32-bit forms Windows puts on the clipboard
// for a screen grab are handled.
func decodeDIB(buf []byte) (Image, error) {
	hdrSize := unsafe.Sizeof(winapi.BITMAPINFOHEADER{})
	if uintptr(len(buf)) < hdrSize {
		return Image{}, errors.New("capture: DIB shorter than its header")
	}
	hdr := (*winapi.BITMAPINFOHEADER)(unsafe.Pointer(&buf[0]))

	w := int(hdr.BiWidth)
	h := int(hdr.BiHeight)
	bottomUp := h > 0
	if h < 0 {
		h = -h
	}
	if w <= 0 || h <= 0 || w > 65536 || h > 65536 {
		return Image{}, fmt.Errorf("capture: implausible DIB size %dx%d", w, h)
	}
	if hdr.BiCompression != winapi.BI_RGB {
		return Image{}, fmt.Errorf("capture: unsupported DIB compression %d", hdr.BiCompression)
	}
	bpp := int(hdr.BiBitCount)
	if bpp != 24 && bpp != 32 {
		return Image{}, fmt.Errorf("capture: unsupported DIB depth %d", bpp)
	}

	offset := int(hdr.BiSize)
	if hdr.BiClrUsed > 0 {
		offset += int(hdr.BiClrUsed) * 4
	}
	stride := ((w*bpp + 31) / 32) * 4
	if offset+stride*h > len(buf) {
		return Image{}, errors.New("capture: DIB pixel data is truncated")
	}
	pix := buf[offset:]

	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		srcRow := y
		if bottomUp {
			srcRow = h - 1 - y
		}
		s := srcRow * stride
		d := y * w * 4
		for x := 0; x < w; x++ {
			if bpp == 32 {
				out[d+x*4+0] = pix[s+x*4+0]
				out[d+x*4+1] = pix[s+x*4+1]
				out[d+x*4+2] = pix[s+x*4+2]
			} else {
				out[d+x*4+0] = pix[s+x*3+0]
				out[d+x*4+1] = pix[s+x*3+1]
				out[d+x*4+2] = pix[s+x*3+2]
			}
			out[d+x*4+3] = 0xFF
		}
	}
	return Image{Pix: out, W: w, H: h}, nil
}
