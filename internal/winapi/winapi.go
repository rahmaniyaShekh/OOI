//go:build windows

// Package winapi holds the thin Win32 bindings the overlay needs.
// Everything is bound lazily so the package can be imported (and its pure
// helpers tested) without the DLLs being touched.
package winapi

import (
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------- constants

const (
	WS_POPUP    = 0x80000000
	WS_VISIBLE  = 0x10000000
	WS_DISABLED = 0x08000000

	WS_EX_LAYERED     = 0x00080000
	WS_EX_TOPMOST     = 0x00000008
	WS_EX_TOOLWINDOW  = 0x00000080
	WS_EX_NOACTIVATE  = 0x08000000
	WS_EX_TRANSPARENT = 0x00000020

	// SetWindowDisplayAffinity values.
	WDA_NONE               = 0x00000000
	WDA_MONITOR            = 0x00000001
	WDA_EXCLUDEFROMCAPTURE = 0x00000011

	ULW_ALPHA    = 0x00000002
	AC_SRC_OVER  = 0x00
	AC_SRC_ALPHA = 0x01

	BI_RGB         = 0
	DIB_RGB_COLORS = 0

	SRCCOPY    = 0x00CC0020
	CAPTUREBLT = 0x40000000

	WM_DESTROY = 0x0002
	WM_CLOSE   = 0x0010
	WM_QUIT    = 0x0012
	WM_HOTKEY  = 0x0312
	WM_APP     = 0x8000

	// Custom messages posted to the overlay window from other goroutines.
	WM_APP_FRAME   = WM_APP + 1
	WM_APP_QUIT    = WM_APP + 2
	WM_APP_UPDATE  = WM_APP + 3
	WM_APP_PROTECT = WM_APP + 4
	WM_APP_PRESENT = WM_APP + 5

	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010
	SWP_SHOWWINDOW = 0x0040

	SW_HIDE   = 0
	SW_SHOWNA = 8

	PM_NOREMOVE = 0x0000
	PM_REMOVE   = 0x0001

	GWL_EXSTYLE = -20

	MOD_ALT      = 0x0001
	MOD_CONTROL  = 0x0002
	MOD_SHIFT    = 0x0004
	MOD_NOREPEAT = 0x4000

	VK_LEFT      = 0x25
	VK_UP        = 0x26
	VK_RIGHT     = 0x27
	VK_DOWN      = 0x28
	VK_OEM_PLUS  = 0xBB
	VK_OEM_MINUS = 0xBD

	IDC_ARROW = 32512

	PW_RENDERFULLCONTENT = 0x00000002

	MONITOR_DEFAULTTOPRIMARY = 0x00000001
	MONITOR_DEFAULTTONEAREST = 0x00000002

	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	dpiPerMonitorAwareV2 = ^uintptr(3) // (HANDLE)-4

	HWND_TOPMOST = ^uintptr(0) // (HWND)-1

	// CreateProcess flags used for detached mode.
	DETACHED_PROCESS         = 0x00000008
	CREATE_NO_WINDOW         = 0x08000000
	CREATE_NEW_PROCESS_GROUP = 0x00000200
)

// ------------------------------------------------------------------ structs

type POINT struct{ X, Y int32 }

type SIZE struct{ CX, CY int32 }

type RECT struct{ Left, Top, Right, Bottom int32 }

// Width returns the horizontal extent of the rectangle.
func (r RECT) Width() int32 { return r.Right - r.Left }

// Height returns the vertical extent of the rectangle.
func (r RECT) Height() int32 { return r.Bottom - r.Top }

type BLENDFUNCTION struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type BITMAPINFOHEADER struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type BITMAPINFO struct {
	Header BITMAPINFOHEADER
	Colors [1]uint32
}

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type MONITORINFO struct {
	CbSize    uint32
	RcMonitor RECT
	RcWork    RECT
	DwFlags   uint32
}

// ------------------------------------------------------------------- procs

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW          = user32.NewProc("RegisterClassExW")
	procUnregisterClassW          = user32.NewProc("UnregisterClassW")
	procCreateWindowExW           = user32.NewProc("CreateWindowExW")
	procDestroyWindow             = user32.NewProc("DestroyWindow")
	procDefWindowProcW            = user32.NewProc("DefWindowProcW")
	procGetMessageW               = user32.NewProc("GetMessageW")
	procPeekMessageW              = user32.NewProc("PeekMessageW")
	procTranslateMessage          = user32.NewProc("TranslateMessage")
	procDispatchMessageW          = user32.NewProc("DispatchMessageW")
	procPostQuitMessage           = user32.NewProc("PostQuitMessage")
	procPostMessageW              = user32.NewProc("PostMessageW")
	procSetWindowDisplayAffinity  = user32.NewProc("SetWindowDisplayAffinity")
	procGetWindowDisplayAffinity  = user32.NewProc("GetWindowDisplayAffinity")
	procUpdateLayeredWindow       = user32.NewProc("UpdateLayeredWindow")
	procSetWindowPos              = user32.NewProc("SetWindowPos")
	procShowWindow                = user32.NewProc("ShowWindow")
	procGetWindowLongPtrW         = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW         = user32.NewProc("SetWindowLongPtrW")
	procRegisterHotKey            = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey          = user32.NewProc("UnregisterHotKey")
	procGetDC                     = user32.NewProc("GetDC")
	procReleaseDC                 = user32.NewProc("ReleaseDC")
	procLoadCursorW               = user32.NewProc("LoadCursorW")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procMonitorFromPoint          = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procPrintWindow               = user32.NewProc("PrintWindow")
	procGetWindowRect             = user32.NewProc("GetWindowRect")
	procGetDesktopWindow          = user32.NewProc("GetDesktopWindow")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
)

// ---------------------------------------------------------------- wrappers

func RegisterClassEx(wc *WNDCLASSEXW) (uint16, error) {
	r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(wc)))
	if r == 0 {
		return 0, e
	}
	return uint16(r), nil
}

func UnregisterClass(name *uint16, hInstance uintptr) {
	procUnregisterClassW.Call(uintptr(unsafe.Pointer(name)), hInstance)
}

func CreateWindowEx(exStyle uint32, class, title *uint16, style uint32, x, y, w, h int32, parent, menu, inst uintptr) (uintptr, error) {
	r, _, e := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(title)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, menu, inst, 0,
	)
	if r == 0 {
		return 0, e
	}
	return r, nil
}

func DestroyWindow(hwnd uintptr) { procDestroyWindow.Call(hwnd) }
func PostQuitMessage(code int32) { procPostQuitMessage.Call(uintptr(code)) }
func DefWindowProc(hwnd, msg, wp, lp uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func PostMessage(hwnd uintptr, msg uint32, wp, lp uintptr) bool {
	r, _, _ := procPostMessageW.Call(hwnd, uintptr(msg), wp, lp)
	return r != 0
}

// GetMessage returns 1 for a normal message, 0 for WM_QUIT and -1 on error.
func GetMessage(m *MSG) int32 {
	r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)
	return int32(r)
}

// PeekMessage removes or inspects a pending message without blocking.
func PeekMessage(m *MSG, hwnd uintptr, minMsg, maxMsg, removeMsg uint32) bool {
	r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(m)), hwnd,
		uintptr(minMsg), uintptr(maxMsg), uintptr(removeMsg))
	return r != 0
}

func TranslateMessage(m *MSG) { procTranslateMessage.Call(uintptr(unsafe.Pointer(m))) }
func DispatchMessage(m *MSG)  { procDispatchMessageW.Call(uintptr(unsafe.Pointer(m))) }

// SetWindowDisplayAffinity is the core of the capture protection.
func SetWindowDisplayAffinity(hwnd uintptr, affinity uint32) error {
	r, _, e := procSetWindowDisplayAffinity.Call(hwnd, uintptr(affinity))
	if r == 0 {
		return e
	}
	return nil
}

func GetWindowDisplayAffinity(hwnd uintptr) (uint32, error) {
	var a uint32
	r, _, e := procGetWindowDisplayAffinity.Call(hwnd, uintptr(unsafe.Pointer(&a)))
	if r == 0 {
		return 0, e
	}
	return a, nil
}

func UpdateLayeredWindow(hwnd, hdcDst uintptr, pptDst *POINT, psize *SIZE, hdcSrc uintptr, pptSrc *POINT, crKey uint32, blend *BLENDFUNCTION, flags uint32) error {
	r, _, e := procUpdateLayeredWindow.Call(
		hwnd, hdcDst,
		uintptr(unsafe.Pointer(pptDst)),
		uintptr(unsafe.Pointer(psize)),
		hdcSrc,
		uintptr(unsafe.Pointer(pptSrc)),
		uintptr(crKey),
		uintptr(unsafe.Pointer(blend)),
		uintptr(flags),
	)
	if r == 0 {
		return e
	}
	return nil
}

func SetWindowPos(hwnd, after uintptr, x, y, cx, cy int32, flags uint32) {
	procSetWindowPos.Call(hwnd, after, uintptr(x), uintptr(y), uintptr(cx), uintptr(cy), uintptr(flags))
}

func ShowWindow(hwnd uintptr, cmd int32) { procShowWindow.Call(hwnd, uintptr(cmd)) }

func GetWindowLongPtr(hwnd uintptr, index int32) uintptr {
	r, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(index))
	return r
}

func SetWindowLongPtr(hwnd uintptr, index int32, val uintptr) uintptr {
	r, _, _ := procSetWindowLongPtrW.Call(hwnd, uintptr(index), val)
	return r
}

func RegisterHotKey(hwnd uintptr, id int32, mods, vk uint32) bool {
	r, _, _ := procRegisterHotKey.Call(hwnd, uintptr(id), uintptr(mods), uintptr(vk))
	return r != 0
}

func UnregisterHotKey(hwnd uintptr, id int32) { procUnregisterHotKey.Call(hwnd, uintptr(id)) }

func GetDC(hwnd uintptr) uintptr {
	r, _, _ := procGetDC.Call(hwnd)
	return r
}

func ReleaseDC(hwnd, hdc uintptr) { procReleaseDC.Call(hwnd, hdc) }

func LoadCursor(id uint16) uintptr {
	r, _, _ := procLoadCursorW.Call(0, uintptr(id))
	return r
}

func GetSystemMetrics(index int32) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(r)
}

func MonitorFromPoint(pt POINT, flags uint32) uintptr {
	r, _, _ := procMonitorFromPoint.Call(uintptr(pt.X), uintptr(pt.Y), uintptr(flags))
	return r
}

func GetMonitorInfo(mon uintptr) (MONITORINFO, bool) {
	mi := MONITORINFO{CbSize: uint32(unsafe.Sizeof(MONITORINFO{}))}
	r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	return mi, r != 0
}

func PrintWindow(hwnd, hdc uintptr, flags uint32) bool {
	r, _, _ := procPrintWindow.Call(hwnd, hdc, uintptr(flags))
	return r != 0
}

func GetWindowRect(hwnd uintptr) (RECT, bool) {
	var r RECT
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r, ret != 0
}

func GetDesktopWindow() uintptr {
	r, _, _ := procGetDesktopWindow.Call()
	return r
}

// EnablePerMonitorDPI makes all coordinates real device pixels so the overlay
// lands exactly where the caller asked for it on scaled displays.
func EnablePerMonitorDPI() {
	if procSetProcessDpiAwarenessCtx.Find() == nil {
		procSetProcessDpiAwarenessCtx.Call(dpiPerMonitorAwareV2)
	}
}

func GetModuleHandle() uintptr {
	r, _, _ := procGetModuleHandleW.Call(0)
	return r
}

func GetConsoleWindow() uintptr {
	r, _, _ := procGetConsoleWindow.Call()
	return r
}

func FreeConsole() { procFreeConsole.Call() }

// --------------------------------------------------------------- GDI procs

func CreateCompatibleDC(hdc uintptr) uintptr {
	r, _, _ := procCreateCompatibleDC.Call(hdc)
	return r
}

func DeleteDC(hdc uintptr) { procDeleteDC.Call(hdc) }

func CreateCompatibleBitmap(hdc uintptr, w, h int32) uintptr {
	r, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	return r
}

func CreateDIBSection(hdc uintptr, bi *BITMAPINFO, usage uint32, bits *unsafe.Pointer, section uintptr, offset uint32) (uintptr, error) {
	r, _, e := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(bi)),
		uintptr(usage),
		uintptr(unsafe.Pointer(bits)),
		section,
		uintptr(offset),
	)
	if r == 0 {
		return 0, e
	}
	return r, nil
}

func DeleteObject(obj uintptr) { procDeleteObject.Call(obj) }

func SelectObject(hdc, obj uintptr) uintptr {
	r, _, _ := procSelectObject.Call(hdc, obj)
	return r
}

func BitBlt(dst uintptr, dx, dy, w, h int32, src uintptr, sx, sy int32, rop uint32) bool {
	r, _, _ := procBitBlt.Call(dst, uintptr(dx), uintptr(dy), uintptr(w), uintptr(h),
		src, uintptr(sx), uintptr(sy), uintptr(rop))
	return r != 0
}

func GetDIBits(hdc, hbm uintptr, start, lines uint32, bits unsafe.Pointer, bi *BITMAPINFO, usage uint32) int32 {
	r, _, _ := procGetDIBits.Call(hdc, hbm, uintptr(start), uintptr(lines),
		uintptr(bits), uintptr(unsafe.Pointer(bi)), uintptr(usage))
	return int32(r)
}

// UTF16Ptr is a panic-free convenience wrapper.
func UTF16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		p, _ = syscall.UTF16PtrFromString("")
	}
	return p
}
