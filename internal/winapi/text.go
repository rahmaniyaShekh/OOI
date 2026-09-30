//go:build windows

package winapi

import "unsafe"

// Text-drawing and timer bindings, kept separate from the core window API.

const (
	WM_TIMER = 0x0113

	TRANSPARENT = 1
	OPAQUE      = 2

	DT_LEFT         = 0x00000000
	DT_CENTER       = 0x00000001
	DT_VCENTER      = 0x00000004
	DT_WORDBREAK    = 0x00000010
	DT_SINGLELINE   = 0x00000020
	DT_NOPREFIX     = 0x00000800
	DT_END_ELLIPSIS = 0x00008000

	FW_NORMAL   = 400
	FW_SEMIBOLD = 600

	DEFAULT_CHARSET     = 1
	OUT_DEFAULT_PRECIS  = 0
	CLIP_DEFAULT_PRECIS = 0
	CLEARTYPE_QUALITY   = 5
	DEFAULT_PITCH       = 0
	FF_DONTCARE         = 0
)

var (
	procSetTimer     = user32.NewProc("SetTimer")
	procKillTimer    = user32.NewProc("KillTimer")
	procDrawTextW    = user32.NewProc("DrawTextW")
	procCreateFontW  = gdi32.NewProc("CreateFontW")
	procSetBkMode    = gdi32.NewProc("SetBkMode")
	procSetTextColor = gdi32.NewProc("SetTextColor")
)

func SetTimer(hwnd uintptr, id uintptr, ms uint32) uintptr {
	r, _, _ := procSetTimer.Call(hwnd, id, uintptr(ms), 0)
	return r
}

func KillTimer(hwnd uintptr, id uintptr) { procKillTimer.Call(hwnd, id) }

// CreateFont creates a simple named font at the given pixel height.
func CreateFont(height int32, weight int32, face string) uintptr {
	r, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight),
		0, 0, 0,
		DEFAULT_CHARSET, OUT_DEFAULT_PRECIS, CLIP_DEFAULT_PRECIS,
		CLEARTYPE_QUALITY, DEFAULT_PITCH|FF_DONTCARE,
		uintptr(unsafe.Pointer(UTF16Ptr(face))),
	)
	return r
}

func SetBkMode(hdc uintptr, mode int32) { procSetBkMode.Call(hdc, uintptr(mode)) }

// SetTextColor takes a COLORREF, which is 0x00BBGGRR.
func SetTextColor(hdc uintptr, colorref uint32) { procSetTextColor.Call(hdc, uintptr(colorref)) }

// DrawText renders text into rect on hdc.
func DrawText(hdc uintptr, text string, rect *RECT, format uint32) {
	u := UTF16Ptr(text)
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u)), ^uintptr(0),
		uintptr(unsafe.Pointer(rect)), uintptr(format))
}

// RGB packs a colour into a COLORREF.
func RGB(r, g, b byte) uint32 {
	return uint32(r) | uint32(g)<<8 | uint32(b)<<16
}
