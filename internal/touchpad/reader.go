//go:build windows

package touchpad

import (
	"fmt"
	"sync"
	"time"

	"ooi/internal/winapi"
)

// Reader turns WM_INPUT messages into assembled touch frames.
//
// It is driven from the window thread: the owner registers for raw input,
// then hands every WM_INPUT lParam to Handle. Devices are discovered lazily,
// the first time a report arrives from them, so hot-plugging an external
// Precision Touchpad works with no extra bookkeeping.
type Reader struct {
	mu      sync.Mutex
	devices map[uintptr]*deviceState
	// buf is reused across WM_INPUT messages so the hot path does not
	// allocate; touch reports arrive at up to a few hundred hertz.
	buf      []byte
	contacts []Contact

	// failed records devices that could not be parsed, so a device that is
	// not really a touchpad is rejected once rather than on every report.
	failed map[uintptr]bool

	frames int64
	errs   int64
}

type deviceState struct {
	dev *Device
	asm Assembler
}

// NewReader creates an idle reader. Call Register once a window exists.
func NewReader() *Reader {
	return &Reader{
		devices: make(map[uintptr]*deviceState),
		failed:  make(map[uintptr]bool),
	}
}

// Register subscribes hwnd to Precision Touchpad raw input.
//
// RIDEV_INPUTSINK is required because the overlay is a WS_EX_NOACTIVATE tool
// window that never takes focus; without it no input would ever arrive.
func (r *Reader) Register(hwnd uintptr) error {
	return winapi.RegisterRawInput(hwnd,
		winapi.HID_USAGE_PAGE_DIGITIZER,
		winapi.HID_USAGE_DIGITIZER_TOUCH_PAD,
		winapi.RIDEV_INPUTSINK)
}

// Unregister drops the subscription.
func (r *Reader) Unregister() {
	winapi.UnregisterRawInput(
		winapi.HID_USAGE_PAGE_DIGITIZER,
		winapi.HID_USAGE_DIGITIZER_TOUCH_PAD)
}

// Stats returns counters useful for diagnosing a silent touchpad.
func (r *Reader) Stats() (frames, errors int64, devices int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames, r.errs, len(r.devices)
}

// DeviceNames lists the touchpads seen so far.
func (r *Reader) DeviceNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d.dev.Name())
	}
	return out
}

// Handle decodes one WM_INPUT and invokes emit for every completed frame.
//
// emit runs on the calling (window) thread. It must not block.
func (r *Reader) Handle(lParam uintptr, at time.Time, emit func(Frame)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pkt, buf, err := winapi.GetRawInputHID(lParam, r.buf)
	r.buf = buf
	if err != nil {
		r.errs++
		return
	}
	if pkt.Count == 0 || pkt.Device == 0 {
		return
	}

	st, ok := r.devices[pkt.Device]
	if !ok {
		if r.failed[pkt.Device] {
			return
		}
		dev, derr := NewDevice(pkt.Device)
		if derr != nil {
			// Not a touchpad we can interpret. Remember it so we do not pay
			// the parsing cost on every subsequent report.
			r.failed[pkt.Device] = true
			r.errs++
			return
		}
		st = &deviceState{dev: dev}
		r.devices[pkt.Device] = st
	}

	for i := 0; i < pkt.Count; i++ {
		report := pkt.ReportAt(i)
		if report == nil {
			continue
		}
		contacts, count, ok := st.dev.DecodeReport(report, r.contacts)
		r.contacts = contacts[:0:cap(contacts)]
		if !ok {
			continue
		}
		if frame, complete := st.asm.Push(contacts, count, at); complete {
			r.frames++
			emit(frame)
		}
	}
}

// Available reports whether this machine has a Precision Touchpad that raw
// input can deliver, without requiring a window.
//
// A false result is not an error: the caller falls back to hotkeys.
func Available() (bool, error) {
	list, err := winapi.RawInputDevices()
	if err != nil {
		return false, err
	}
	for _, d := range list {
		if d.Type != winapi.RIM_TYPEHID {
			continue
		}
		info, err := winapi.RawInputDeviceInfo(d.Device)
		if err != nil {
			continue
		}
		if info.UsagePage == winapi.HID_USAGE_PAGE_DIGITIZER &&
			info.Usage == winapi.HID_USAGE_DIGITIZER_TOUCH_PAD {
			return true, nil
		}
	}
	return false, fmt.Errorf("no Precision Touchpad found")
}
