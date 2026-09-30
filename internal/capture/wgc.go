//go:build windows

package capture

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Windows.Graphics.Capture (WGC).
//
// This is the capture API that matters most in 2024-era Windows: Xbox Game
// Bar, the Snipping Tool, Teams, Discord, Slack, Zoom and OBS's "Window
// Capture (WGC)" / "Display Capture (WGC)" all go through it. It is also the
// only mainstream API that can capture a *specific window* on the composited
// desktop, so it is the one most likely to be pointed at the overlay.
//
// The whole thing is WinRT, which means IInspectable-derived interfaces
// (three extra vtable slots before the interface's own methods) and activation
// factories obtained by runtime class name. It is written out by hand here to
// keep the project dependency-free.

var (
	combase                = syscall.NewLazyDLL("combase.dll")
	procRoInitialize       = combase.NewProc("RoInitialize")
	procRoGetActivationFac = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateStr   = combase.NewProc("WindowsCreateString")
	procWindowsDeleteStr   = combase.NewProc("WindowsDeleteString")

	procCreateD3D11DevFromDXGI = d3d11.NewProc("CreateDirect3D11DeviceFromDXGIDevice")

	user32Wgc            = syscall.NewLazyDLL("user32.dll")
	procMonitorFromPoint = user32Wgc.NewProc("MonitorFromPoint")
	procMonitorFromWnd   = user32Wgc.NewProc("MonitorFromWindow")
)

// IInspectable adds GetIids/GetRuntimeClassName/GetTrustLevel after IUnknown,
// so a WinRT interface's own methods begin at slot 6.
const inspectableBase = 6

const (
	// IGraphicsCaptureItemInterop (classic COM, not IInspectable)
	idxCreateForWindow  = 3
	idxCreateForMonitor = 4

	// IGraphicsCaptureItem
	idxItemGetSize = inspectableBase + 1

	// IDirect3D11CaptureFramePoolStatics2
	idxCreateFreeThreaded = inspectableBase + 0

	// IDirect3D11CaptureFramePool
	idxTryGetNextFrame      = inspectableBase + 1
	idxCreateCaptureSession = inspectableBase + 4

	// IGraphicsCaptureSession
	idxStartCapture = inspectableBase + 0

	// IDirect3D11CaptureFrame
	idxFrameGetSurface = inspectableBase + 0

	// IDirect3DDxgiInterfaceAccess (classic COM)
	idxGetInterface = 3

	// IClosable
	idxClose = inspectableBase + 0
)

const (
	roInitMultithreaded = 1
	// DirectXPixelFormat.B8G8R8A8UIntNormalized
	pixelFormatBGRA8 = 87
	// RPC_E_CHANGED_MODE: the thread already has a different apartment, which
	// is harmless for our purposes.
	rpcEChangedMode = 0x80010106
	sFalse          = 1

	monitorDefaultToPrimary = 0x00000001
	monitorDefaultToNearest = 0x00000002
)

var (
	iidGraphicsCaptureItemInterop = guid{0x3628E81B, 0x3CAC, 0x4C60, [8]byte{0xB7, 0xF4, 0x23, 0xCE, 0x0E, 0x0C, 0x33, 0x56}}
	iidGraphicsCaptureItem        = guid{0x79C3F95B, 0x31F7, 0x4EC2, [8]byte{0xA4, 0x64, 0x63, 0x2E, 0xF5, 0xD3, 0x07, 0x60}}
	iidFramePoolStatics2          = guid{0x589B103F, 0x6BBC, 0x5DF5, [8]byte{0xA9, 0x91, 0x02, 0xE2, 0x8B, 0x3B, 0x66, 0xD5}}
	iidDxgiInterfaceAccess        = guid{0xA9B3D012, 0x3DF2, 0x4EE3, [8]byte{0xB8, 0xD1, 0x86, 0x95, 0xF4, 0x57, 0xD3, 0xC1}}
	iidClosable                   = guid{0x30D5A829, 0x7FA4, 0x4026, [8]byte{0x83, 0xBB, 0xD7, 0x5B, 0xAE, 0x4E, 0xA9, 0x9E}}
)

// sizeInt32 is Windows.Graphics.SizeInt32.
type sizeInt32 struct{ Width, Height int32 }

// packSize encodes a SizeInt32 the way the x64 ABI passes it by value: two
// int32 fields packed into one 8-byte register.
func packSize(s sizeInt32) uintptr {
	return uintptr(uint64(uint32(s.Width)) | uint64(uint32(s.Height))<<32)
}

// hstring is an opaque WinRT string handle.
type hstring uintptr

// newHString creates an HSTRING the caller must delete.
func newHString(s string) (hstring, error) {
	u16, err := syscall.UTF16FromString(s)
	if err != nil {
		return 0, fmt.Errorf("capture: bad runtime class name %q: %w", s, err)
	}
	var h hstring
	// len-1 excludes the terminating NUL that UTF16FromString appends.
	hr, _, _ := procWindowsCreateStr.Call(
		uintptr(unsafe.Pointer(&u16[0])),
		uintptr(len(u16)-1),
		uintptr(unsafe.Pointer(&h)),
	)
	runtime.KeepAlive(u16)
	if hr != 0 {
		return 0, hresultErr("WindowsCreateString", hr)
	}
	return h, nil
}

func (h hstring) delete() {
	if h != 0 {
		procWindowsDeleteStr.Call(uintptr(h))
	}
}

// wgcSupported reports whether the WGC runtime classes are present at all.
func wgcSupported() error {
	if err := procRoGetActivationFac.Find(); err != nil {
		return fmt.Errorf("capture: WinRT unavailable: %w", err)
	}
	if err := procCreateD3D11DevFromDXGI.Find(); err != nil {
		return fmt.Errorf("capture: CreateDirect3D11DeviceFromDXGIDevice unavailable: %w", err)
	}
	return nil
}

// activationFactory resolves a WinRT activation factory by class name.
func activationFactory(class string, iid *guid) (unsafe.Pointer, error) {
	h, err := newHString(class)
	if err != nil {
		return nil, err
	}
	defer h.delete()

	var factory unsafe.Pointer
	hr, _, _ := procRoGetActivationFac.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 || factory == nil {
		return nil, hresultErr("RoGetActivationFactory("+class+")", hr)
	}
	return factory, nil
}

// WGCTarget selects what a WGC session captures.
type WGCTarget int

const (
	// WGCMonitor captures the whole display containing a point.
	WGCMonitor WGCTarget = iota
	// WGCWindow captures one specific window, occluded or not.
	WGCWindow
)

// WGCSession is an open Windows.Graphics.Capture session.
type WGCSession struct {
	device    unsafe.Pointer // ID3D11Device
	context   unsafe.Pointer // ID3D11DeviceContext
	rtDevice  unsafe.Pointer // IDirect3DDevice (IInspectable)
	item      unsafe.Pointer // IGraphicsCaptureItem
	framePool unsafe.Pointer // IDirect3D11CaptureFramePool
	session   unsafe.Pointer // IGraphicsCaptureSession
	staging   unsafe.Pointer // ID3D11Texture2D

	// Size is the capture item's reported size in physical pixels.
	Size sizeInt32
	// Origin is where the captured image sits in virtual-desktop
	// coordinates. For a window capture this is the window's top-left.
	Origin struct{ X, Y int }
}

// NewWGCSession opens a capture session.
//
// For WGCMonitor, px/py select the display. For WGCWindow, hwnd is captured
// directly and px/py are ignored.
func NewWGCSession(target WGCTarget, hwnd uintptr, px, py int) (*WGCSession, error) {
	if err := wgcSupported(); err != nil {
		return nil, err
	}

	// WinRT is per-thread. Multithreaded apartment matches the free-threaded
	// frame pool used below.
	if hr, _, _ := procRoInitialize.Call(roInitMultithreaded); hr != 0 &&
		uint32(hr) != rpcEChangedMode && hr != sFalse {
		return nil, hresultErr("RoInitialize", hr)
	}

	s := &WGCSession{}
	if err := s.build(target, hwnd, px, py); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *WGCSession) build(target WGCTarget, hwnd uintptr, px, py int) error {
	// ---- D3D11 device, then its WinRT projection ------------------------
	hr, _, _ := procD3D11CreateDev.Call(
		0, d3dDriverTypeHardware, 0, 0, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&s.device)), 0, uintptr(unsafe.Pointer(&s.context)),
	)
	if hr != 0 || s.device == nil {
		return hresultErr("D3D11CreateDevice", hr)
	}

	var dxgiDev unsafe.Pointer
	if hr := comCall(s.device, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidIDXGIDevice)), uintptr(unsafe.Pointer(&dxgiDev))); hr != 0 {
		return hresultErr("QueryInterface(IDXGIDevice)", hr)
	}
	defer release(&dxgiDev)

	hr, _, _ = procCreateD3D11DevFromDXGI.Call(
		uintptr(dxgiDev), uintptr(unsafe.Pointer(&s.rtDevice)))
	if hr != 0 || s.rtDevice == nil {
		return hresultErr("CreateDirect3D11DeviceFromDXGIDevice", hr)
	}

	// ---- the capture item -----------------------------------------------
	interop, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureItem",
		&iidGraphicsCaptureItemInterop)
	if err != nil {
		return err
	}
	defer release(&interop)

	switch target {
	case WGCWindow:
		if hwnd == 0 {
			return fmt.Errorf("capture: WGC window capture needs a window handle")
		}
		hr = comCall(interop, idxCreateForWindow, hwnd,
			uintptr(unsafe.Pointer(&iidGraphicsCaptureItem)), uintptr(unsafe.Pointer(&s.item)))
		if hr != 0 || s.item == nil {
			return hresultErr("IGraphicsCaptureItemInterop::CreateForWindow", hr)
		}
		if r, ok := getWindowRectRaw(hwnd); ok {
			s.Origin.X, s.Origin.Y = int(r.Left), int(r.Top)
		}

	default:
		mon := monitorFromPoint(px, py)
		if mon == 0 {
			return fmt.Errorf("capture: no monitor at (%d,%d)", px, py)
		}
		hr = comCall(interop, idxCreateForMonitor, mon,
			uintptr(unsafe.Pointer(&iidGraphicsCaptureItem)), uintptr(unsafe.Pointer(&s.item)))
		if hr != 0 || s.item == nil {
			return hresultErr("IGraphicsCaptureItemInterop::CreateForMonitor", hr)
		}
		if l, t, ok := monitorOrigin(mon); ok {
			s.Origin.X, s.Origin.Y = l, t
		}
	}

	if hr := comCall(s.item, idxItemGetSize, uintptr(unsafe.Pointer(&s.Size))); hr != 0 {
		return hresultErr("IGraphicsCaptureItem::get_Size", hr)
	}
	runtime.KeepAlive(&s.Size)
	if s.Size.Width <= 0 || s.Size.Height <= 0 {
		return fmt.Errorf("capture: capture item reports a %dx%d size", s.Size.Width, s.Size.Height)
	}

	// ---- frame pool and session -----------------------------------------
	statics, err := activationFactory("Windows.Graphics.Capture.Direct3D11CaptureFramePool",
		&iidFramePoolStatics2)
	if err != nil {
		return fmt.Errorf("capture: free-threaded frame pool unavailable (needs Windows 10 1903+): %w", err)
	}
	defer release(&statics)

	// CreateFreeThreaded avoids needing a DispatcherQueue on this thread.
	//
	// SizeInt32 is an 8-byte POD, so the x64 ABI passes it *by value* in a
	// single register rather than by pointer. Passing its address here fails
	// with E_INVALIDARG.
	hr = comCall(statics, idxCreateFreeThreaded,
		uintptr(s.rtDevice),
		pixelFormatBGRA8,
		2, // buffers
		packSize(s.Size),
		uintptr(unsafe.Pointer(&s.framePool)),
	)
	if hr != 0 || s.framePool == nil {
		return hresultErr("Direct3D11CaptureFramePool::CreateFreeThreaded", hr)
	}

	if hr := comCall(s.framePool, idxCreateCaptureSession,
		uintptr(s.item), uintptr(unsafe.Pointer(&s.session))); hr != 0 || s.session == nil {
		return hresultErr("CreateCaptureSession", hr)
	}
	if hr := comCall(s.session, idxStartCapture); hr != 0 {
		return hresultErr("StartCapture", hr)
	}
	return nil
}

// Grab polls the frame pool until a frame arrives or the timeout elapses.
func (s *WGCSession) Grab(timeout time.Duration) (Image, error) {
	deadline := time.Now().Add(timeout)
	for {
		var frame unsafe.Pointer
		hr := comCall(s.framePool, idxTryGetNextFrame, uintptr(unsafe.Pointer(&frame)))
		if hr != 0 {
			return Image{}, hresultErr("TryGetNextFrame", hr)
		}
		if frame == nil {
			if time.Now().After(deadline) {
				return Image{}, fmt.Errorf("capture: WGC produced no frame within %s", timeout)
			}
			time.Sleep(15 * time.Millisecond)
			continue
		}

		img, err := s.readFrame(frame)
		closeAndRelease(&frame)
		if err != nil {
			return Image{}, err
		}
		return img, nil
	}
}

// readFrame pulls the ID3D11Texture2D out of a WinRT capture frame.
func (s *WGCSession) readFrame(frame unsafe.Pointer) (Image, error) {
	var surface unsafe.Pointer
	if hr := comCall(frame, idxFrameGetSurface, uintptr(unsafe.Pointer(&surface))); hr != 0 || surface == nil {
		return Image{}, hresultErr("IDirect3D11CaptureFrame::get_Surface", hr)
	}
	defer release(&surface)

	var access unsafe.Pointer
	if hr := comCall(surface, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidDxgiInterfaceAccess)), uintptr(unsafe.Pointer(&access))); hr != 0 {
		return Image{}, hresultErr("QueryInterface(IDirect3DDxgiInterfaceAccess)", hr)
	}
	defer release(&access)

	var tex unsafe.Pointer
	if hr := comCall(access, idxGetInterface,
		uintptr(unsafe.Pointer(&iidID3D11Texture)), uintptr(unsafe.Pointer(&tex))); hr != 0 || tex == nil {
		return Image{}, hresultErr("IDirect3DDxgiInterfaceAccess::GetInterface", hr)
	}
	defer release(&tex)

	return textureToImage(s.device, s.context, tex, &s.staging)
}

// closeAndRelease calls IClosable::Close when supported, then releases.
func closeAndRelease(obj *unsafe.Pointer) {
	if *obj == nil {
		return
	}
	var closable unsafe.Pointer
	if hr := comCall(*obj, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidClosable)), uintptr(unsafe.Pointer(&closable))); hr == 0 && closable != nil {
		comCall(closable, idxClose)
		release(&closable)
	}
	release(obj)
}

// Close tears the session down.
func (s *WGCSession) Close() {
	release(&s.staging)
	closeAndRelease(&s.session)
	closeAndRelease(&s.framePool)
	release(&s.item)
	release(&s.rtDevice)
	release(&s.context)
	release(&s.device)
}

// WGCScreen captures the display containing (px, py) via WGC.
// It returns the image and the origin it maps to on the virtual desktop.
func WGCScreen(px, py int, timeout time.Duration) (Image, int, int, error) {
	// WinRT apartment state is per-thread, so pin the goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	s, err := NewWGCSession(WGCMonitor, 0, px, py)
	if err != nil {
		return Image{}, 0, 0, err
	}
	defer s.Close()

	img, err := s.Grab(timeout)
	return img, s.Origin.X, s.Origin.Y, err
}

// WGCWindowCapture captures a single window via WGC. This is the API path
// used by "share a window" in Teams, Discord and Zoom.
func WGCWindowCapture(hwnd uintptr, timeout time.Duration) (Image, int, int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	s, err := NewWGCSession(WGCWindow, hwnd, 0, 0)
	if err != nil {
		return Image{}, 0, 0, err
	}
	defer s.Close()

	img, err := s.Grab(timeout)
	return img, s.Origin.X, s.Origin.Y, err
}

// ---------------------------------------------------------------- helpers

func monitorFromPoint(x, y int) uintptr {
	// MonitorFromPoint takes POINT by value, which is two int32s packed into
	// one register on amd64.
	pt := uint64(uint32(int32(x))) | uint64(uint32(int32(y)))<<32
	r, _, _ := procMonitorFromPoint.Call(uintptr(pt), monitorDefaultToNearest)
	return r
}

type monitorInfoRaw struct {
	CbSize    uint32
	RcMonitor Rect32
	RcWork    Rect32
	DwFlags   uint32
}

func monitorOrigin(mon uintptr) (int, int, bool) {
	getInfo := user32Wgc.NewProc("GetMonitorInfoW")
	mi := monitorInfoRaw{CbSize: uint32(unsafe.Sizeof(monitorInfoRaw{}))}
	r, _, _ := getInfo.Call(mon, uintptr(unsafe.Pointer(&mi)))
	runtime.KeepAlive(&mi)
	if r == 0 {
		return 0, 0, false
	}
	return int(mi.RcMonitor.Left), int(mi.RcMonitor.Top), true
}

func getWindowRectRaw(hwnd uintptr) (Rect32, bool) {
	getRect := user32Wgc.NewProc("GetWindowRect")
	var rc Rect32
	r, _, _ := getRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	runtime.KeepAlive(&rc)
	return rc, r != 0
}
