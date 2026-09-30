//go:build windows

package main

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"ooi/internal/winapi"
)

// A message-only window: invisible, never composited, but able to receive
// WM_INPUT. Used by `probe-touchpad`, which needs a raw input target without
// putting anything on screen.

const (
	msgOnlyClass = "OOIMessageOnly"
	hwndMessage  = ^uintptr(2) // HWND_MESSAGE is (HWND)-3
)

var (
	msgOnlyOnce sync.Once
	msgOnlyProc uintptr
	msgOnlyMu   sync.Mutex
	msgOnlyMap  = map[uintptr]func(msg, wp, lp uintptr) bool{}
)

// newMessageOnlyWindow creates a hidden window and routes its messages to fn.
// fn returns true when it has handled the message.
func newMessageOnlyWindow(fn func(msg, wp, lp uintptr) bool) (uintptr, func(), error) {
	msgOnlyOnce.Do(func() {
		msgOnlyProc = syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
			msgOnlyMu.Lock()
			h := msgOnlyMap[hwnd]
			msgOnlyMu.Unlock()
			if h != nil && h(msg, wp, lp) {
				return 0
			}
			return winapi.DefWindowProc(hwnd, msg, wp, lp)
		})
	})

	inst := winapi.GetModuleHandle()
	class := winapi.UTF16Ptr(msgOnlyClass)
	wc := winapi.WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(winapi.WNDCLASSEXW{})),
		LpfnWndProc:   msgOnlyProc,
		HInstance:     inst,
		LpszClassName: class,
	}
	// A duplicate registration from an earlier call in this process is fine.
	if _, err := winapi.RegisterClassEx(&wc); err != nil {
		var errno syscall.Errno
		const errClassAlreadyExists = syscall.Errno(1410)
		if !(As(err, &errno) && errno == errClassAlreadyExists) {
			return 0, nil, fmt.Errorf("register message window class: %w", err)
		}
	}

	// A real top-level window, parked far off-screen, rather than a
	// message-only (HWND_MESSAGE) window. RIDEV_INPUTSINK delivery to
	// message-only windows is unreliable, and this costs nothing: the window
	// is never shown, so it is never composited.
	hwnd, err := winapi.CreateWindowEx(
		winapi.WS_EX_TOOLWINDOW|winapi.WS_EX_NOACTIVATE,
		class, winapi.UTF16Ptr("ooi-probe"),
		winapi.WS_POPUP,
		-32000, -32000, 1, 1,
		0, 0, inst)
	if err != nil {
		return 0, nil, fmt.Errorf("create message window: %w", err)
	}

	msgOnlyMu.Lock()
	msgOnlyMap[hwnd] = fn
	msgOnlyMu.Unlock()

	return hwnd, func() {
		msgOnlyMu.Lock()
		delete(msgOnlyMap, hwnd)
		msgOnlyMu.Unlock()
		winapi.DestroyWindow(hwnd)
	}, nil
}

// As is a tiny errors.As shim kept local so this file has no extra imports.
func As(err error, target *syscall.Errno) bool {
	if e, ok := err.(syscall.Errno); ok {
		*target = e
		return true
	}
	return false
}

// pumpMessages runs a message loop for a fixed duration.
func pumpMessages(d time.Duration) {
	deadline := time.Now().Add(d)
	var msg winapi.MSG
	for time.Now().Before(deadline) {
		// PeekMessage keeps the loop responsive to the deadline; GetMessage
		// would block indefinitely when the touchpad is idle.
		for winapi.PeekMessage(&msg, 0, 0, 0, winapi.PM_REMOVE) {
			if msg.Message == winapi.WM_QUIT {
				return
			}
			winapi.TranslateMessage(&msg)
			winapi.DispatchMessage(&msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
