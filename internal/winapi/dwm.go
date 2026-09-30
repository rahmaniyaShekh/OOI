//go:build windows

package winapi

import (
	"syscall"
	"unsafe"
)

// Desktop Window Manager attributes used to keep the overlay's pixels from
// surfacing anywhere other than the physical display.
const (
	DWMWA_TRANSITIONS_FORCEDISABLED = 3  // no show/hide animation of a stale frame
	DWMWA_DISALLOW_PEEK             = 11 // no Aero Peek preview of this window
	DWMWA_EXCLUDED_FROM_PEEK        = 12 // not revealed when peeking the desktop
	DWMWA_CLOAK                     = 13 // DWM composes the window nowhere
	DWMWA_CLOAKED                   = 14 // read-only: why (if) it is cloaked

	WM_APP_ENABLE = WM_APP + 6 // wParam 1 = show, 0 = hide
)

var (
	dwmapi                    = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
)

// DwmSetBool sets a BOOL-valued DWM window attribute.
func DwmSetBool(hwnd uintptr, attr uint32, on bool) error {
	var v int32
	if on {
		v = 1
	}
	hr, _, _ := procDwmSetWindowAttribute.Call(hwnd, uintptr(attr), uintptr(unsafe.Pointer(&v)), 4)
	if hr != 0 {
		return syscall.Errno(hr)
	}
	return nil
}

// DwmCloaked reads DWMWA_CLOAKED back from the compositor. Non-zero means the
// window is cloaked (1 = by this app).
func DwmCloaked(hwnd uintptr) (uint32, error) {
	var v uint32
	hr, _, _ := procDwmGetWindowAttribute.Call(hwnd, DWMWA_CLOAKED, uintptr(unsafe.Pointer(&v)), 4)
	if hr != 0 {
		return 0, syscall.Errno(hr)
	}
	return v, nil
}
