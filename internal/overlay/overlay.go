//go:build windows

// Package overlay owns the on-screen window that displays the remote screen
// and is excluded from screen capture.
//
// The protection itself is one call: SetWindowDisplayAffinity with
// WDA_EXCLUDEFROMCAPTURE (0x11), available since Windows 10 build 19041. DWM
// then composites the window to the physical display but omits it from every
// software capture path -- GDI BitBlt of the screen DC, PrintWindow, DXGI
// Desktop Duplication, and Windows.Graphics.Capture. It does not, and cannot,
// hide the window from a camera or an HDMI capture device.
//
// Threading: Win32 windows are owned by the thread that created them, so Run
// locks an OS thread and drives the message loop. Every other goroutine
// interacts by stashing state under a mutex and posting a message.
package overlay

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"ooi/internal/frame"
	"ooi/internal/gesture"
	"ooi/internal/winapi"
)

const className = "OOIProtectedSurface"

// Hotkey identifiers.
const (
	hkToggleVisible = 1
	hkQuit          = 2
	hkClickThrough  = 3
	hkProtection    = 4
	hkMoveLeft      = 5
	hkMoveUp        = 6
	hkMoveRight     = 7
	hkMoveDown      = 8
	hkOpacityUp     = 9
	hkOpacityDown   = 10
	hkGrowW         = 11
	hkShrinkW       = 12
	hkGrowH         = 13
	hkShrinkH       = 14
)

const (
	timerTopmost = 1 // re-assert topmost; fullscreen apps can steal it
	moveStep     = 24
	sizeStep     = 32
	opacityStep  = 16
)

// Config describes the initial overlay state.
type Config struct {
	X, Y         int
	W, H         int
	Opacity      int // 1..255
	ClickThrough bool
	Protect      bool
	Hotkeys      bool
	// Placeholder is shown until the first frame arrives.
	Placeholder string

	// Gestures enables touchpad control of opacity and position.
	Gestures bool
	// GestureConfig tunes recognition; the zero value uses the defaults.
	GestureConfig gesture.Config

	// OnVisibility, when set, is told whenever the overlay is enabled or
	// disabled (by a gesture, a hotkey, or fail-closed protection).
	OnVisibility func(visible bool)
}

// Overlay is a protected, layered, always-on-top window.
// It satisfies server.Sink.
type Overlay struct {
	cfg Config

	hwnd atomic.Uintptr

	// affinityRestored counts how many times the watchdog had to put the
	// capture exclusion back. It should stay at zero.
	affinityRestored atomic.Uint64

	// failClosedN counts refusals to show because protection could not be
	// confirmed. It should stay at zero.
	failClosedN atomic.Uint64

	// UI-thread-only state.
	memDC  uintptr
	bitmap uintptr
	oldBmp uintptr
	canvas frame.Canvas
	font   uintptr

	// Shared state.
	mu       sync.Mutex
	pending  image.Image
	status   string
	hasFrame bool
	visible  bool
	protect  bool
	clickThr bool
	opacity  int
	x, y     int
	w, h     int

	// Marker mode paints a flat, unmistakable colour. It exists so the
	// capture verifier has something it can search a screenshot for.
	markerOn      bool
	mkB, mkG, mkR byte
	markerLabel   string

	// protectResult receives the outcome of the next SetProtect request.
	protectResult chan error

	ready     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	// Non-fatal errors observed on the UI thread, surfaced by Run.
	protectErr error

	// Gesture subsystem, owned by the UI thread.
	gest       *gestureState
	gestureErr error
}

// New creates an Overlay. It does not touch Win32 until Run is called.
func New(cfg Config) *Overlay {
	if cfg.Opacity <= 0 || cfg.Opacity > 255 {
		cfg.Opacity = 255
	}
	if cfg.Placeholder == "" {
		cfg.Placeholder = "waiting for sender"
	}
	return &Overlay{
		cfg:      cfg,
		visible:  true,
		protect:  cfg.Protect,
		clickThr: cfg.ClickThrough,
		opacity:  cfg.Opacity,
		x:        cfg.X,
		y:        cfg.Y,
		w:        cfg.W,
		h:        cfg.H,
		status:   cfg.Placeholder,
		ready:    make(chan struct{}),
		closed:   make(chan struct{}),
	}
}

// Ready is closed once the window exists and the first present has happened.
func (o *Overlay) Ready() <-chan struct{} { return o.ready }

// Closed is closed when the message loop has exited.
func (o *Overlay) Closed() <-chan struct{} { return o.closed }

// HWND returns the window handle, or 0 before Run creates it.
func (o *Overlay) HWND() uintptr { return o.hwnd.Load() }

// AffinityRestored reports how many times the watchdog had to re-apply
// capture exclusion. A non-zero value means something external cleared it.
func (o *Overlay) AffinityRestored() uint64 { return o.affinityRestored.Load() }

// Protected reports whether the window currently carries capture exclusion,
// read back from Windows rather than from local state.
func (o *Overlay) Protected() bool {
	hwnd := o.hwnd.Load()
	if hwnd == 0 {
		return false
	}
	aff, err := winapi.GetWindowDisplayAffinity(hwnd)
	return err == nil && aff == winapi.WDA_EXCLUDEFROMCAPTURE
}

// ---------------------------------------------------------------- Sink impl

// Size reports the current overlay dimensions.
func (o *Overlay) Size() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w, o.h
}

// Frame queues a decoded frame for display. Only the newest frame survives:
// if the UI thread has not caught up, the older frame is dropped rather than
// queued, which keeps latency flat under load.
func (o *Overlay) Frame(img image.Image) {
	o.mu.Lock()
	o.pending = img
	o.hasFrame = true
	o.mu.Unlock()

	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_FRAME, 0, 0)
	}
}

// Status updates the placeholder text shown when no frame is displayed.
func (o *Overlay) Status(state string) {
	o.mu.Lock()
	o.status = state
	hasFrame := o.hasFrame
	o.mu.Unlock()

	if hasFrame {
		return // do not paint over live video
	}
	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_UPDATE, 0, 0)
	}
}

// ClearFrame drops the displayed image and returns to the placeholder.
func (o *Overlay) ClearFrame() {
	o.mu.Lock()
	o.pending = nil
	o.hasFrame = false
	o.mu.Unlock()
	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_UPDATE, 0, 0)
	}
}

// SetOpacity sets the window-wide alpha, clamped to the legal range.
//
// This is the same path a touchpad ramp takes: it re-blends the existing
// window through UpdateLayeredWindow. The HWND is never touched, so the
// capture exclusion attached to it cannot be affected.
func (o *Overlay) SetOpacity(v int) {
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	o.mu.Lock()
	o.opacity = v
	if o.gest != nil {
		o.gest.opacityF = float64(v)
	}
	o.mu.Unlock()
	o.requestPresent()
}

// Opacity returns the current window-wide alpha.
func (o *Overlay) Opacity() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opacity
}

// MoveBy shifts the overlay, keeping it reachable on the virtual screen.
// Like SetOpacity this repositions the existing window rather than making a
// new one.
func (o *Overlay) MoveBy(dx, dy int) {
	o.mu.Lock()
	o.x += dx
	o.y += dy
	o.clampToVirtualScreenLocked()
	o.mu.Unlock()
	o.requestPresent()
}

// Position returns the overlay's top-left corner.
func (o *Overlay) Position() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.x, o.y
}

// requestPresent asks the window thread to re-blend. Presenting from another
// goroutine would touch GDI objects owned by the window thread.
func (o *Overlay) requestPresent() {
	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_PRESENT, 0, 0)
	}
}

// SetMarker switches the overlay to a flat colour and repaints. Used by the
// capture verifier; opacity is forced to fully opaque so the captured pixels
// are exactly this colour if the window is visible to the capturer at all.
//
// label is drawn across the middle so anyone looking at the screen -- or at a
// screenshot of it -- can tell which phase of the test they are seeing.
func (o *Overlay) SetMarker(b, g, r byte, label string) {
	o.mu.Lock()
	o.markerOn = true
	o.mkB, o.mkG, o.mkR = b, g, r
	o.markerLabel = label
	o.opacity = 255
	o.hasFrame = false
	o.pending = nil
	o.mu.Unlock()

	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_UPDATE, 0, 0)
	}
}

// SetProtect turns capture exclusion on or off at runtime. The change is
// applied on the UI thread and the returned channel reports the outcome.
func (o *Overlay) SetProtect(on bool) <-chan error {
	res := make(chan error, 1)
	hwnd := o.hwnd.Load()
	if hwnd == 0 {
		res <- errors.New("overlay: window not created")
		return res
	}
	o.mu.Lock()
	o.protect = on
	o.protectResult = res
	o.mu.Unlock()

	var wp uintptr
	if on {
		wp = 1
	}
	winapi.PostMessage(hwnd, winapi.WM_APP_PROTECT, wp, 0)
	return res
}

// Stop asks the message loop to exit. Safe from any goroutine.
func (o *Overlay) Stop() {
	if hwnd := o.hwnd.Load(); hwnd != 0 {
		winapi.PostMessage(hwnd, winapi.WM_APP_QUIT, 0, 0)
		return
	}
	o.markClosed()
}

func (o *Overlay) markClosed() {
	o.closeOnce.Do(func() { close(o.closed) })
}

// ---------------------------------------------------------------- registry

// Windows callbacks carry no user pointer, so map HWND back to the Overlay.
var registry sync.Map // uintptr -> *Overlay

func lookup(hwnd uintptr) *Overlay {
	if v, ok := registry.Load(hwnd); ok {
		return v.(*Overlay)
	}
	return nil
}

// wndProcCallback is created once; syscall.NewCallback leaks by design.
var wndProcCallback = func() uintptr {
	return newCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		if o := lookup(hwnd); o != nil {
			if handled, ret := o.handle(msg, wp, lp); handled {
				return ret
			}
		}
		return winapi.DefWindowProc(hwnd, msg, wp, lp)
	})
}()

// ------------------------------------------------------------------- Run

// Run creates the window and pumps messages until Stop is called or the
// window is destroyed. It must be called from the goroutine that owns the
// overlay; it locks the OS thread for its whole lifetime.
func (o *Overlay) Run() (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer o.markClosed()

	winapi.EnablePerMonitorDPI()

	inst := winapi.GetModuleHandle()
	classPtr := winapi.UTF16Ptr(className)

	wc := winapi.WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(winapi.WNDCLASSEXW{})),
		LpfnWndProc:   wndProcCallback,
		HInstance:     inst,
		HCursor:       winapi.LoadCursor(winapi.IDC_ARROW),
		LpszClassName: classPtr,
	}
	// A duplicate class registration is fine (a previous Run in this process
	// already registered it); anything else is fatal.
	if _, rerr := winapi.RegisterClassEx(&wc); rerr != nil && !isClassAlreadyExists(rerr) {
		return fmt.Errorf("overlay: register class: %w", rerr)
	}

	exStyle := uint32(winapi.WS_EX_LAYERED | winapi.WS_EX_TOPMOST |
		winapi.WS_EX_TOOLWINDOW | winapi.WS_EX_NOACTIVATE)
	if o.clickThr {
		exStyle |= winapi.WS_EX_TRANSPARENT
	}

	hwnd, cerr := winapi.CreateWindowEx(
		exStyle, classPtr, winapi.UTF16Ptr("ooi"),
		winapi.WS_POPUP,
		int32(o.x), int32(o.y), int32(o.w), int32(o.h),
		0, 0, inst,
	)
	if cerr != nil {
		return fmt.Errorf("overlay: create window: %w", cerr)
	}
	registry.Store(hwnd, o)
	o.hwnd.Store(hwnd)

	defer func() {
		registry.Delete(hwnd)
		o.hwnd.Store(0)
		o.releaseSurface()
		if o.font != 0 {
			winapi.DeleteObject(o.font)
			o.font = 0
		}
	}()

	// Apply capture protection before the window is ever shown, so it is
	// never visible to a recorder even for one frame.
	if o.protect {
		if perr := winapi.SetWindowDisplayAffinity(hwnd, winapi.WDA_EXCLUDEFROMCAPTURE); perr != nil {
			o.protectErr = fmt.Errorf("overlay: SetWindowDisplayAffinity: %w", perr)
			return o.protectErr
		}
	}

	// Keep the compositor from ever showing these pixels anywhere but the
	// display: no show/hide animation (which would replay a stale frame), and
	// no Aero Peek preview. Best effort; protection does not depend on them.
	winapi.DwmSetBool(hwnd, winapi.DWMWA_TRANSITIONS_FORCEDISABLED, true)
	winapi.DwmSetBool(hwnd, winapi.DWMWA_DISALLOW_PEEK, true)
	winapi.DwmSetBool(hwnd, winapi.DWMWA_EXCLUDED_FROM_PEEK, true)

	if aerr := o.allocSurface(o.w, o.h); aerr != nil {
		return aerr
	}

	if o.cfg.Hotkeys {
		o.registerHotkeys(hwnd)
		defer o.unregisterHotkeys(hwnd)
	}

	// Gesture input is set up after the window exists and after protection
	// is applied, so there is no window in which a gesture could act on an
	// unprotected surface.
	o.initGestures(hwnd)
	defer o.shutdownGestures(hwnd)

	winapi.ShowWindow(hwnd, winapi.SW_SHOWNA)
	winapi.SetWindowPos(hwnd, winapi.HWND_TOPMOST, 0, 0, 0, 0,
		winapi.SWP_NOMOVE|winapi.SWP_NOSIZE|winapi.SWP_NOACTIVATE)

	o.redraw()
	close(o.ready)

	winapi.SetTimer(hwnd, timerTopmost, 2000)
	defer winapi.KillTimer(hwnd, timerTopmost)

	var msg winapi.MSG
	for {
		r := winapi.GetMessage(&msg)
		if r == 0 {
			return nil // WM_QUIT
		}
		if r == -1 {
			return errors.New("overlay: GetMessage failed")
		}
		winapi.TranslateMessage(&msg)
		winapi.DispatchMessage(&msg)
	}
}

// handle processes messages for this window. It runs on the UI thread.
func (o *Overlay) handle(msg, wp, lp uintptr) (bool, uintptr) {
	switch msg {
	case winapi.WM_APP_FRAME, winapi.WM_APP_UPDATE:
		o.redraw()
		return true, 0

	case winapi.WM_APP_PRESENT:
		// Opacity and position changes only need a re-blend, not a repaint
		// of the surface contents.
		o.present()
		return true, 0

	case winapi.WM_APP_QUIT:
		winapi.PostQuitMessage(0)
		return true, 0

	case winapi.WM_APP_ENABLE:
		o.setEnabledUI(wp != 0)
		return true, 0

	case winapi.WM_APP_PROTECT:
		aff := uint32(winapi.WDA_NONE)
		if wp != 0 {
			aff = winapi.WDA_EXCLUDEFROMCAPTURE
		}
		err := winapi.SetWindowDisplayAffinity(o.hwnd.Load(), aff)
		o.mu.Lock()
		res := o.protectResult
		o.protectResult = nil
		o.mu.Unlock()
		if res != nil {
			res <- err
		}
		return true, 0

	case winapi.WM_TIMER:
		if wp == timerTopmost {
			o.mu.Lock()
			vis := o.visible
			wantProtect := o.protect
			o.mu.Unlock()
			hwnd := o.hwnd.Load()

			if vis {
				winapi.SetWindowPos(hwnd, winapi.HWND_TOPMOST, 0, 0, 0, 0,
					winapi.SWP_NOMOVE|winapi.SWP_NOSIZE|winapi.SWP_NOACTIVATE)
			}
			// Re-assert capture exclusion. Nothing in this program clears
			// it, but the cost of checking is negligible and the cost of it
			// being silently off is the entire point of the tool.
			if wantProtect {
				if aff, err := winapi.GetWindowDisplayAffinity(hwnd); err == nil &&
					aff != winapi.WDA_EXCLUDEFROMCAPTURE {
					winapi.SetWindowDisplayAffinity(hwnd, winapi.WDA_EXCLUDEFROMCAPTURE)
					o.affinityRestored.Add(1)
				}
			}
			return true, 0
		}
		if wp == timerGesture {
			o.onGestureTimer()
			return true, 0
		}

	case winapi.WM_HOTKEY:
		o.onHotkey(int32(wp))
		return true, 0

	case winapi.WM_INPUT:
		o.onRawInput(lp)
		// Raw input must still reach DefWindowProc so the system can clean
		// up the packet.
		return false, 0

	case winapi.WM_CLOSE:
		winapi.PostQuitMessage(0)
		return true, 0

	case winapi.WM_DESTROY:
		winapi.PostQuitMessage(0)
		return true, 0
	}
	return false, 0
}

// ------------------------------------------------------------ DIB surface

// allocSurface creates the 32-bit top-down DIB the overlay renders into.
func (o *Overlay) allocSurface(w, h int) error {
	o.releaseSurface()

	screen := winapi.GetDC(0)
	if screen == 0 {
		return errors.New("overlay: GetDC(0) failed")
	}
	dc := winapi.CreateCompatibleDC(screen)
	winapi.ReleaseDC(0, screen)
	if dc == 0 {
		return errors.New("overlay: CreateCompatibleDC failed")
	}

	bi := winapi.BITMAPINFO{
		Header: winapi.BITMAPINFOHEADER{
			BiSize:        uint32(unsafe.Sizeof(winapi.BITMAPINFOHEADER{})),
			BiWidth:       int32(w),
			BiHeight:      -int32(h), // negative => top-down rows
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: winapi.BI_RGB,
		},
	}
	var bits unsafe.Pointer
	bmp, err := winapi.CreateDIBSection(dc, &bi, winapi.DIB_RGB_COLORS, &bits, 0, 0)
	if err != nil || bits == nil {
		winapi.DeleteDC(dc)
		return fmt.Errorf("overlay: CreateDIBSection: %w", err)
	}

	o.memDC = dc
	o.bitmap = bmp
	o.oldBmp = winapi.SelectObject(dc, bmp)
	o.canvas = frame.Canvas{
		Pix: unsafe.Slice((*byte)(bits), w*h*4),
		W:   w,
		H:   h,
	}
	return nil
}

func (o *Overlay) releaseSurface() {
	if o.memDC != 0 {
		if o.oldBmp != 0 {
			winapi.SelectObject(o.memDC, o.oldBmp)
			o.oldBmp = 0
		}
		winapi.DeleteDC(o.memDC)
		o.memDC = 0
	}
	if o.bitmap != 0 {
		winapi.DeleteObject(o.bitmap)
		o.bitmap = 0
	}
	o.canvas = frame.Canvas{}
}

// redraw repaints the surface from current state and presents it.
func (o *Overlay) redraw() {
	o.mu.Lock()
	img := o.pending
	hasFrame := o.hasFrame
	status := o.status
	w, h := o.w, o.h
	marker := o.markerOn
	mb, mg, mr := o.mkB, o.mkG, o.mkR
	label := o.markerLabel
	visible := o.visible
	o.mu.Unlock()

	// While disabled the surface is left wiped and is never repainted. The
	// newest frame is kept in memory (o.pending) so enabling shows the current
	// screen at once, but no pixel of it is written anywhere DWM can read.
	if !visible {
		return
	}

	if !o.canvas.Valid() || o.canvas.W != w || o.canvas.H != h {
		if err := o.allocSurface(w, h); err != nil {
			return
		}
	}

	switch {
	case marker:
		o.canvas.FillBGRA(mb, mg, mr)
		if label != "" {
			o.drawLabel(label, winapi.RGB(0xFF, 0xFF, 0xFF), -26)
		}
	case hasFrame && img != nil:
		o.canvas.Draw(img)
	default:
		o.drawPlaceholder(status)
	}
	o.present()
}

// present pushes the DIB to the screen through UpdateLayeredWindow, which is
// what gives the window per-pixel alpha without a WM_PAINT cycle.
func (o *Overlay) present() {
	hwnd := o.hwnd.Load()
	if hwnd == 0 || o.memDC == 0 {
		return
	}
	o.mu.Lock()
	pos := winapi.POINT{X: int32(o.x), Y: int32(o.y)}
	size := winapi.SIZE{CX: int32(o.w), CY: int32(o.h)}
	alpha := byte(o.opacity)
	visible := o.visible
	protect := o.protect
	o.mu.Unlock()

	if !visible {
		return
	}
	// Fail closed: confirm the capture exclusion with the OS before any pixel
	// is pushed. If it cannot be confirmed the overlay takes itself off screen
	// rather than present one unprotected frame.
	if protect && !o.ensureProtected(hwnd) {
		o.failClosed(hwnd)
		return
	}
	o.pushSurface(hwnd, pos, size, alpha)
}

// pushSurface hands the DIB to the compositor. It is the only place that calls
// UpdateLayeredWindow.
func (o *Overlay) pushSurface(hwnd uintptr, pos winapi.POINT, size winapi.SIZE, alpha byte) {
	src := winapi.POINT{}
	blend := winapi.BLENDFUNCTION{
		BlendOp:             winapi.AC_SRC_OVER,
		SourceConstantAlpha: alpha,
		AlphaFormat:         winapi.AC_SRC_ALPHA,
	}
	winapi.UpdateLayeredWindow(hwnd, 0, &pos, &size, o.memDC, &src, 0, &blend, winapi.ULW_ALPHA)
}

// drawPlaceholder paints the idle card: a dark panel, an accent border and a
// line of status text.
//
// GDI text drawing writes zero into the alpha channel, which would be
// invisible under premultiplied alpha, so the whole panel is forced opaque
// afterwards. That is correct here precisely because the panel is opaque.
func (o *Overlay) drawPlaceholder(text string) {
	if !o.canvas.Valid() {
		return
	}
	o.canvas.FillBGRA(0x18, 0x12, 0x0d) // #0d1218-ish, B,G,R order

	// 2px accent border.
	const border = 2
	w, h := o.canvas.W, o.canvas.H
	setPx := func(x, y int) {
		i := (y*w + x) * 4
		o.canvas.Pix[i+0] = 0x60 // B
		o.canvas.Pix[i+1] = 0xb9 // G
		o.canvas.Pix[i+2] = 0x3f // R  -> #3fb960
		o.canvas.Pix[i+3] = 0xFF
	}
	for t := 0; t < border; t++ {
		if t >= h || t >= w {
			break
		}
		for x := 0; x < w; x++ {
			setPx(x, t)
			setPx(x, h-1-t)
		}
		for y := 0; y < h; y++ {
			setPx(t, y)
			setPx(w-1-t, y)
		}
	}

	o.drawLabel(text, winapi.RGB(0xc9, 0xd1, 0xd9), -18)
}

// drawLabel renders one centred line of text onto the canvas.
//
// GDI text drawing writes zero into the alpha channel, which would be
// invisible under premultiplied alpha, so the surface is forced opaque
// afterwards. That is correct wherever this is used, because both callers
// paint an opaque background first.
func (o *Overlay) drawLabel(text string, colour uint32, height int32) {
	if o.memDC == 0 || !o.canvas.Valid() || text == "" {
		return
	}
	if o.font == 0 {
		o.font = winapi.CreateFont(height, winapi.FW_SEMIBOLD, "Segoe UI")
	}
	if o.font != 0 {
		old := winapi.SelectObject(o.memDC, o.font)
		defer winapi.SelectObject(o.memDC, old)
	}
	winapi.SetBkMode(o.memDC, winapi.TRANSPARENT)
	winapi.SetTextColor(o.memDC, colour)

	r := winapi.RECT{Left: 12, Top: 0, Right: int32(o.canvas.W - 12), Bottom: int32(o.canvas.H)}
	winapi.DrawText(o.memDC, text, &r,
		winapi.DT_CENTER|winapi.DT_VCENTER|winapi.DT_WORDBREAK|winapi.DT_NOPREFIX)

	pix := o.canvas.Pix
	for i := 3; i < len(pix); i += 4 {
		pix[i] = 0xFF
	}
}

// ------------------------------------------------------------------ hotkeys

func (o *Overlay) registerHotkeys(hwnd uintptr) {
	const ctrlAlt = winapi.MOD_CONTROL | winapi.MOD_ALT
	const ctrlAltShift = ctrlAlt | winapi.MOD_SHIFT

	// Toggles use MOD_NOREPEAT; movement deliberately does not, so holding a
	// key nudges the overlay repeatedly.
	//
	// There is deliberately no hotkey for capture protection. A stray
	// keypress must never be able to expose the overlay to a recorder; the
	// only code that turns it off is the verifier, which needs an
	// unprotected control phase to make its measurement meaningful.
	winapi.RegisterHotKey(hwnd, hkToggleVisible, ctrlAlt|winapi.MOD_NOREPEAT, 'H')
	winapi.RegisterHotKey(hwnd, hkQuit, ctrlAlt|winapi.MOD_NOREPEAT, 'Q')
	winapi.RegisterHotKey(hwnd, hkClickThrough, ctrlAlt|winapi.MOD_NOREPEAT, 'M')

	winapi.RegisterHotKey(hwnd, hkMoveLeft, ctrlAlt, winapi.VK_LEFT)
	winapi.RegisterHotKey(hwnd, hkMoveUp, ctrlAlt, winapi.VK_UP)
	winapi.RegisterHotKey(hwnd, hkMoveRight, ctrlAlt, winapi.VK_RIGHT)
	winapi.RegisterHotKey(hwnd, hkMoveDown, ctrlAlt, winapi.VK_DOWN)

	winapi.RegisterHotKey(hwnd, hkOpacityUp, ctrlAlt, winapi.VK_OEM_PLUS)
	winapi.RegisterHotKey(hwnd, hkOpacityDown, ctrlAlt, winapi.VK_OEM_MINUS)

	winapi.RegisterHotKey(hwnd, hkGrowW, ctrlAltShift, winapi.VK_RIGHT)
	winapi.RegisterHotKey(hwnd, hkShrinkW, ctrlAltShift, winapi.VK_LEFT)
	winapi.RegisterHotKey(hwnd, hkGrowH, ctrlAltShift, winapi.VK_DOWN)
	winapi.RegisterHotKey(hwnd, hkShrinkH, ctrlAltShift, winapi.VK_UP)
}

func (o *Overlay) unregisterHotkeys(hwnd uintptr) {
	for id := int32(hkToggleVisible); id <= hkShrinkH; id++ {
		winapi.UnregisterHotKey(hwnd, id)
	}
}

func (o *Overlay) onHotkey(id int32) {
	hwnd := o.hwnd.Load()
	switch id {
	case hkQuit:
		winapi.PostQuitMessage(0)

	case hkToggleVisible:
		// The hotkey and the tap gestures share one path, so they can never
		// disagree about whether the overlay is on screen.
		o.mu.Lock()
		vis := o.visible
		o.mu.Unlock()
		o.setEnabledUI(!vis)

	case hkClickThrough:
		o.mu.Lock()
		o.clickThr = !o.clickThr
		on := o.clickThr
		vis := o.visible
		o.mu.Unlock()
		// While disabled the window stays click-through regardless; the new
		// preference applies when it comes back.
		if vis {
			o.setClickThrough(hwnd, on)
		}

	case hkMoveLeft, hkMoveRight, hkMoveUp, hkMoveDown:
		dx, dy := 0, 0
		switch id {
		case hkMoveLeft:
			dx = -moveStep
		case hkMoveRight:
			dx = moveStep
		case hkMoveUp:
			dy = -moveStep
		case hkMoveDown:
			dy = moveStep
		}
		o.mu.Lock()
		o.x += dx
		o.y += dy
		o.mu.Unlock()
		o.present()

	case hkOpacityUp, hkOpacityDown:
		d := opacityStep
		if id == hkOpacityDown {
			d = -opacityStep
		}
		o.mu.Lock()
		o.opacity = clampOpacity(o.opacity + d)
		o.mu.Unlock()
		o.present()

	case hkGrowW, hkShrinkW, hkGrowH, hkShrinkH:
		dw, dh := 0, 0
		switch id {
		case hkGrowW:
			dw = sizeStep
		case hkShrinkW:
			dw = -sizeStep
		case hkGrowH:
			dh = sizeStep
		case hkShrinkH:
			dh = -sizeStep
		}
		o.mu.Lock()
		o.w = clampDim(o.w + dw)
		o.h = clampDim(o.h + dh)
		o.mu.Unlock()
		o.redraw() // reallocates the DIB at the new size
	}
}

func clampOpacity(v int) int {
	if v < 24 {
		return 24
	}
	if v > 255 {
		return 255
	}
	return v
}

func clampDim(v int) int {
	if v < 96 {
		return 96
	}
	if v > 8192 {
		return 8192
	}
	return v
}
