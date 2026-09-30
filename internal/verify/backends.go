//go:build windows

package verify

import (
	"time"

	"ooi/internal/capture"
	"ooi/internal/overlay"
)

// Backends is every screen-capture path the verifier exercises.
//
// The list deliberately covers each distinct mechanism Windows offers rather
// than each product that uses one, because products share mechanisms:
//
//	Windows.Graphics.Capture  Xbox Game Bar, Snipping Tool, Teams, Zoom,
//	                          Discord, Slack, OBS "Window/Display Capture (WGC)"
//	DXGI Desktop Duplication  OBS "Display Capture", most remote desktop tools,
//	                          Parsec, Steam Remote Play, many recorders
//	GDI BitBlt / StretchBlt   older recorders, ShareX, screenshot utilities,
//	                          Chrome's legacy screen capture
//	PrintWindow               per-window grabbers, automation tools,
//	                          older conferencing "share a window"
//	PrintScreen key           the keyboard path through the clipboard
func Backends() []grabber {
	return []grabber{
		{
			name: "WGC monitor capture",
			note: "Game Bar, Snipping Tool, Teams, OBS (WGC)",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				return capture.WGCScreen(r.X+r.W/2, r.Y+r.H/2, 5*time.Second)
			},
		},
		{
			name: "WGC window capture",
			note: "\"share a window\" in Teams/Zoom/Discord",
			// Aimed straight at the overlay's own HWND: the most direct
			// attempt anything can make to record this specific window.
			grab: func(o *overlay.Overlay, _ Rect) (capture.Image, int, int, error) {
				return capture.WGCWindowCapture(o.HWND(), 5*time.Second)
			},
			// A protected window cannot be turned into a capture item at
			// all on current Windows, so failing to start is itself the
			// protected outcome rather than an inconclusive one.
			failureMeansProtected: true,
		},
		{
			name: "DXGI Desktop Duplication",
			note: "OBS display capture, Parsec, remote desktop",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				// A fresh duplication session per grab keeps the two phases
				// independent: no cached surface can carry over.
				img, bounds, err := capture.DXGIScreen(r.X+r.W/2, r.Y+r.H/2, 4*time.Second)
				return img, int(bounds.Left), int(bounds.Top), err
			},
		},
		{
			name: "GDI BitBlt + CAPTUREBLT",
			note: "classic recorders, ShareX",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.GDIScreen(r.X, r.Y, r.W, r.H)
				return img, r.X, r.Y, err
			},
		},
		{
			name: "GDI BitBlt (no CAPTUREBLT)",
			note: "naive recorders; skips layered windows anyway",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.GDIScreenNoCaptureBlt(r.X, r.Y, r.W, r.H)
				return img, r.X, r.Y, err
			},
		},
		{
			name: "GDI StretchBlt",
			note: "scaling capture path",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.GDIStretchBlt(r.X, r.Y, r.W, r.H)
				return img, r.X, r.Y, err
			},
		},
		{
			name: "GDI desktop window DC",
			note: "GetWindowDC(GetDesktopWindow())",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.GDIWindowDC(r.X, r.Y, r.W, r.H)
				return img, r.X, r.Y, err
			},
		},
		{
			name: "PrintWindow (full content)",
			note: "per-window grabbers, automation",
			grab: func(o *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.PrintWindowOf(o.HWND())
				return img, r.X, r.Y, err
			},
			failureMeansProtected: true,
		},
		{
			name: "PrintWindow (legacy WM_PRINT)",
			note: "pre-8.1 window capture",
			grab: func(o *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				img, err := capture.PrintWindowLegacy(o.HWND())
				return img, r.X, r.Y, err
			},
			failureMeansProtected: true,
			// Without PW_RENDERFULLCONTENT this path asks the window to
			// repaint itself via WM_PRINT. The overlay never paints that
			// way -- its pixels come from UpdateLayeredWindow -- so this
			// API captures nothing here whether protected or not.
			blindReason: "cannot capture layered windows at all (no WM_PRINT content)",
		},
		{
			name: "PrintScreen key (clipboard)",
			note: "the keyboard screenshot path",
			grab: func(_ *overlay.Overlay, r Rect) (capture.Image, int, int, error) {
				// The clipboard bitmap covers the whole virtual screen.
				ox, oy, _, _ := capture.ScreenBounds()
				img, err := capture.PrintScreenClipboard(4 * time.Second)
				return img, ox, oy, err
			},
		},
	}
}
