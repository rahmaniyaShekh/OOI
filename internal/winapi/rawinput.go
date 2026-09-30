//go:build windows

package winapi

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Raw Input bindings for reading a Precision Touchpad.
//
// Raw Input is deliberately *passive*: registering for it does not consume or
// alter the events, so the touchpad keeps driving the cursor, taps and
// two-finger scrolling exactly as before. That is what makes it safe to watch
// every contact without breaking normal use of the machine.

const (
	// Digitizer usage page and the Touch Pad usage within it.
	HID_USAGE_PAGE_GENERIC   = 0x01
	HID_USAGE_PAGE_DIGITIZER = 0x0D

	HID_USAGE_GENERIC_X = 0x30
	HID_USAGE_GENERIC_Y = 0x31

	HID_USAGE_DIGITIZER_TOUCH_PAD   = 0x05
	HID_USAGE_DIGITIZER_TIP_SWITCH  = 0x42
	HID_USAGE_DIGITIZER_CONTACT_ID  = 0x51
	HID_USAGE_DIGITIZER_CONTACT_CNT = 0x54

	// RIDEV_INPUTSINK delivers input even when our window is not foreground,
	// which is essential here: the overlay never takes focus.
	RIDEV_INPUTSINK = 0x00000100
	RIDEV_REMOVE    = 0x00000001
	RIDEV_DEVNOTIFY = 0x00002000

	RID_INPUT  = 0x10000003
	RID_HEADER = 0x10000005

	RIDI_PREPARSEDDATA = 0x20000005
	RIDI_DEVICENAME    = 0x20000007
	RIDI_DEVICEINFO    = 0x2000000B

	RIM_TYPEMOUSE    = 0
	RIM_TYPEKEYBOARD = 1
	RIM_TYPEHID      = 2

	WM_INPUT               = 0x00FF
	WM_INPUT_DEVICE_CHANGE = 0x00FE
)

// RAWINPUTDEVICE registers interest in one HID usage.
type RAWINPUTDEVICE struct {
	UsagePage  uint16
	Usage      uint16
	Flags      uint32
	HwndTarget uintptr
}

// RAWINPUTHEADER precedes every raw input packet.
type RAWINPUTHEADER struct {
	Type   uint32
	Size   uint32
	Device uintptr
	WParam uintptr
}

// rawHIDPrefix is the fixed part of RAWHID; the report bytes follow it.
type rawHIDPrefix struct {
	SizeHid uint32
	Count   uint32
}

// rawInputHIDOffset is where the HID report bytes begin inside a RAWINPUT.
const rawInputHIDOffset = unsafe.Sizeof(RAWINPUTHEADER{}) + unsafe.Sizeof(rawHIDPrefix{})

var (
	procRegisterRawInputDevices = user32.NewProc("RegisterRawInputDevices")
	procGetRawInputData         = user32.NewProc("GetRawInputData")
	procGetRawInputDeviceInfoW  = user32.NewProc("GetRawInputDeviceInfoW")
	procGetRawInputDeviceList   = user32.NewProc("GetRawInputDeviceList")
)

// RegisterRawInput subscribes hwnd to a raw input usage.
func RegisterRawInput(hwnd uintptr, usagePage, usage uint16, flags uint32) error {
	rid := RAWINPUTDEVICE{
		UsagePage:  usagePage,
		Usage:      usage,
		Flags:      flags,
		HwndTarget: hwnd,
	}
	r, _, e := procRegisterRawInputDevices.Call(
		uintptr(unsafe.Pointer(&rid)), 1, unsafe.Sizeof(rid))
	if r == 0 {
		return fmt.Errorf("winapi: RegisterRawInputDevices(%#x/%#x): %w", usagePage, usage, e)
	}
	return nil
}

// UnregisterRawInput drops a previous subscription.
func UnregisterRawInput(usagePage, usage uint16) {
	rid := RAWINPUTDEVICE{UsagePage: usagePage, Usage: usage, Flags: RIDEV_REMOVE}
	procRegisterRawInputDevices.Call(uintptr(unsafe.Pointer(&rid)), 1, unsafe.Sizeof(rid))
}

// RawInputPacket is one decoded WM_INPUT payload from a HID device.
type RawInputPacket struct {
	Device uintptr
	// Reports holds Count reports of SizeHid bytes each, concatenated.
	Reports []byte
	SizeHid int
	Count   int
}

// ReportAt returns report i, or nil when out of range.
func (p RawInputPacket) ReportAt(i int) []byte {
	if i < 0 || i >= p.Count || p.SizeHid <= 0 {
		return nil
	}
	start := i * p.SizeHid
	if start+p.SizeHid > len(p.Reports) {
		return nil
	}
	return p.Reports[start : start+p.SizeHid]
}

// GetRawInputHID decodes a WM_INPUT lParam into HID reports.
//
// buf is reused across calls to keep this allocation-free on the hot path; it
// is grown as needed and returned so the caller can keep it.
func GetRawInputHID(hRawInput uintptr, buf []byte) (RawInputPacket, []byte, error) {
	hdrSize := uint32(unsafe.Sizeof(RAWINPUTHEADER{}))

	// First call with a nil buffer asks for the required size.
	var need uint32
	r, _, _ := procGetRawInputData.Call(
		hRawInput, RID_INPUT, 0, uintptr(unsafe.Pointer(&need)), uintptr(hdrSize))
	if int32(r) < 0 || need == 0 {
		return RawInputPacket{}, buf, fmt.Errorf("winapi: GetRawInputData size query failed")
	}
	if uint32(len(buf)) < need {
		buf = make([]byte, need)
	}

	got, _, _ := procGetRawInputData.Call(
		hRawInput, RID_INPUT, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&need)), uintptr(hdrSize))
	if int32(got) < 0 || uint32(got) < hdrSize {
		return RawInputPacket{}, buf, fmt.Errorf("winapi: GetRawInputData failed")
	}

	hdr := (*RAWINPUTHEADER)(unsafe.Pointer(&buf[0]))
	if hdr.Type != RIM_TYPEHID {
		return RawInputPacket{Device: hdr.Device}, buf, nil
	}
	if uintptr(got) < rawInputHIDOffset {
		return RawInputPacket{}, buf, fmt.Errorf("winapi: raw HID packet too short")
	}

	prefix := (*rawHIDPrefix)(unsafe.Pointer(&buf[unsafe.Sizeof(RAWINPUTHEADER{})]))
	sizeHid, count := int(prefix.SizeHid), int(prefix.Count)
	if sizeHid <= 0 || count <= 0 {
		return RawInputPacket{Device: hdr.Device}, buf, nil
	}

	total := sizeHid * count
	avail := int(uintptr(got) - rawInputHIDOffset)
	if total > avail {
		total = avail
		count = total / sizeHid
	}
	return RawInputPacket{
		Device:  hdr.Device,
		Reports: buf[rawInputHIDOffset : int(rawInputHIDOffset)+total],
		SizeHid: sizeHid,
		Count:   count,
	}, buf, nil
}

// GetPreparsedData fetches a device's HID preparsed data, which the HidP_*
// functions need in order to interpret its reports.
func GetPreparsedData(device uintptr) ([]byte, error) {
	var size uint32
	r, _, _ := procGetRawInputDeviceInfoW.Call(
		device, RIDI_PREPARSEDDATA, 0, uintptr(unsafe.Pointer(&size)))
	if int32(r) < 0 || size == 0 {
		return nil, fmt.Errorf("winapi: preparsed data size query failed")
	}
	buf := make([]byte, size)
	got, _, e := procGetRawInputDeviceInfoW.Call(
		device, RIDI_PREPARSEDDATA, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if int32(got) < 0 {
		return nil, fmt.Errorf("winapi: GetRawInputDeviceInfo(PREPARSEDDATA): %w", e)
	}
	return buf, nil
}

// RAWINPUTDEVICELIST is one entry from GetRawInputDeviceList.
type RAWINPUTDEVICELIST struct {
	Device uintptr
	Type   uint32
	_      uint32 // explicit tail padding on amd64
}

// ridDeviceInfoHID is the HID arm of RID_DEVICE_INFO's union.
type ridDeviceInfoHID struct {
	VendorID      uint32
	ProductID     uint32
	VersionNumber uint32
	UsagePage     uint16
	Usage         uint16
}

// ridDeviceInfo mirrors RID_DEVICE_INFO. The union is sized by its largest
// arm (RID_DEVICE_INFO_KEYBOARD, six DWORDs).
type ridDeviceInfo struct {
	Size uint32
	Type uint32
	HID  ridDeviceInfoHID
	_    [8]byte // pad the union out to the keyboard arm's size
}

// HIDInfo describes a HID device's top-level usage.
type HIDInfo struct {
	VendorID  uint32
	ProductID uint32
	UsagePage uint16
	Usage     uint16
}

// RawInputDevices enumerates every raw input device on the system.
func RawInputDevices() ([]RAWINPUTDEVICELIST, error) {
	entry := uint32(unsafe.Sizeof(RAWINPUTDEVICELIST{}))

	var count uint32
	r, _, e := procGetRawInputDeviceList.Call(
		0, uintptr(unsafe.Pointer(&count)), uintptr(entry))
	if int32(r) < 0 {
		return nil, fmt.Errorf("winapi: GetRawInputDeviceList size query: %w", e)
	}
	if count == 0 {
		return nil, nil
	}

	list := make([]RAWINPUTDEVICELIST, count)
	got, _, e := procGetRawInputDeviceList.Call(
		uintptr(unsafe.Pointer(&list[0])), uintptr(unsafe.Pointer(&count)), uintptr(entry))
	if int32(got) < 0 {
		return nil, fmt.Errorf("winapi: GetRawInputDeviceList: %w", e)
	}
	if int(got) < len(list) {
		list = list[:got]
	}
	return list, nil
}

// RawInputDeviceInfo returns a HID device's vendor, product and top-level
// usage, which is how a touchpad is told apart from any other digitizer.
func RawInputDeviceInfo(device uintptr) (HIDInfo, error) {
	info := ridDeviceInfo{Size: uint32(unsafe.Sizeof(ridDeviceInfo{}))}
	size := info.Size
	r, _, e := procGetRawInputDeviceInfoW.Call(
		device, RIDI_DEVICEINFO,
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&size)))
	if int32(r) < 0 {
		return HIDInfo{}, fmt.Errorf("winapi: GetRawInputDeviceInfo(DEVICEINFO): %w", e)
	}
	if info.Type != RIM_TYPEHID {
		return HIDInfo{}, fmt.Errorf("winapi: device is not a HID device")
	}
	return HIDInfo{
		VendorID:  info.HID.VendorID,
		ProductID: info.HID.ProductID,
		UsagePage: info.HID.UsagePage,
		Usage:     info.HID.Usage,
	}, nil
}

// GetRawInputDeviceName returns the device interface path.
func GetRawInputDeviceName(device uintptr) string {
	var size uint32
	r, _, _ := procGetRawInputDeviceInfoW.Call(
		device, RIDI_DEVICENAME, 0, uintptr(unsafe.Pointer(&size)))
	if int32(r) < 0 || size == 0 {
		return ""
	}
	buf := make([]uint16, size+1)
	got, _, _ := procGetRawInputDeviceInfoW.Call(
		device, RIDI_DEVICENAME, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if int32(got) < 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
