//go:build windows

// Package touchpad reads multi-finger contacts from a Windows Precision
// Touchpad via Raw Input.
//
// Portability is the whole point of how this is written. Nothing about any
// specific vendor is hardcoded: the device's own HID report descriptor is
// parsed at runtime with the HidP_* API, finger collections are discovered by
// their standard digitizer usages, and coordinates are normalised to 0..1
// using each device's declared logical range. Any laptop with a Precision
// Touchpad (Windows 8.1+, effectively every laptop since ~2015) works without
// a code change.
//
// Reading is passive. Raw Input does not consume events, so the touchpad
// continues to drive the cursor, taps, and two-finger scrolling exactly as it
// did before.
package touchpad

import (
	"fmt"
	"syscall"
	"unsafe"

	"ooi/internal/winapi"
)

var (
	hid = syscall.NewLazyDLL("hid.dll")

	procHidPGetCaps            = hid.NewProc("HidP_GetCaps")
	procHidPGetValueCaps       = hid.NewProc("HidP_GetValueCaps")
	procHidPGetUsageValue      = hid.NewProc("HidP_GetUsageValue")
	procHidPGetUsages          = hid.NewProc("HidP_GetUsages")
	procHidPMaxUsageListLength = hid.NewProc("HidP_MaxUsageListLength")
)

// HidP report types and status codes.
const (
	hidPInput = 0

	hidpStatusSuccess            = 0x00110000
	hidpStatusUsageNotFound      = 0xC0110004
	hidpStatusIncompatibleReport = 0xC0110016
)

// hidpCaps mirrors HIDP_CAPS (64 bytes).
type hidpCaps struct {
	Usage                     uint16
	UsagePage                 uint16
	InputReportByteLength     uint16
	OutputReportByteLength    uint16
	FeatureReportByteLength   uint16
	Reserved                  [17]uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps     uint16
	NumberInputValueCaps      uint16
	NumberInputDataIndices    uint16
	NumberOutputButtonCaps    uint16
	NumberOutputValueCaps     uint16
	NumberOutputDataIndices   uint16
	NumberFeatureButtonCaps   uint16
	NumberFeatureValueCaps    uint16
	NumberFeatureDataIndices  uint16
}

// hidpValueCaps mirrors HIDP_VALUE_CAPS (72 bytes on both x86 and x64).
//
// The trailing union is flattened: only the NotRange.Usage field is needed,
// and for a range entry the same offset holds UsageMin, which is the correct
// value to key on for the finger collections this package looks for.
type hidpValueCaps struct {
	UsagePage         uint16
	ReportID          uint8
	IsAlias           uint8
	BitField          uint16
	LinkCollection    uint16
	LinkUsage         uint16
	LinkUsagePage     uint16
	IsRange           uint8
	IsStringRange     uint8
	IsDesignatorRange uint8
	IsAbsolute        uint8
	HasNull           uint8
	Reserved          uint8
	BitSize           uint16
	ReportCount       uint16
	Reserved2         [5]uint16
	UnitsExp          uint32
	Units             uint32
	LogicalMin        int32
	LogicalMax        int32
	PhysicalMin       int32
	PhysicalMax       int32
	// Union, 16 bytes. Usage/UsageMin lives at the start of both variants.
	Usage      uint16
	Usage2     uint16
	StringIdx  uint16
	StringIdx2 uint16
	DesigIdx   uint16
	DesigIdx2  uint16
	DataIndex  uint16
	DataIndex2 uint16
}

// ntStatusOK reports whether an NTSTATUS returned success.
func ntStatusOK(r uintptr) bool { return uint32(r) == hidpStatusSuccess }

// fingerSlot describes one finger collection found in the report descriptor.
type fingerSlot struct {
	collection uint16
	// Logical ranges for this slot, used to normalise to 0..1.
	xMin, xMax int32
	yMin, yMax int32
	hasX, hasY bool
}

// Device is a parsed touchpad, ready to decode reports.
type Device struct {
	handle    uintptr
	name      string
	preparsed []byte

	slots []fingerSlot
	// countCollection is where Contact Count lives (usually the top level).
	countCollection uint16
	hasCount        bool

	// usageBuf is reused by HidP_GetUsages to avoid per-report allocation.
	usageBuf []uint16
}

// Name returns the device interface path.
func (d *Device) Name() string { return d.name }

// Slots returns how many simultaneous contacts the descriptor declares.
func (d *Device) Slots() int { return len(d.slots) }

// Handle returns the raw input device handle.
func (d *Device) Handle() uintptr { return d.handle }

// NewDevice inspects a raw input device and, if it is a usable touchpad,
// returns a decoder for it.
func NewDevice(handle uintptr) (*Device, error) {
	preparsed, err := winapi.GetPreparsedData(handle)
	if err != nil {
		return nil, err
	}
	d := &Device{
		handle:    handle,
		name:      winapi.GetRawInputDeviceName(handle),
		preparsed: preparsed,
	}
	if err := d.parseLayout(); err != nil {
		return nil, err
	}
	return d, nil
}

// parseLayout discovers the finger collections from the report descriptor.
func (d *Device) parseLayout() error {
	var caps hidpCaps
	r, _, _ := procHidPGetCaps.Call(
		uintptr(unsafe.Pointer(&d.preparsed[0])), uintptr(unsafe.Pointer(&caps)))
	if !ntStatusOK(r) {
		return fmt.Errorf("touchpad: HidP_GetCaps failed (status %#x)", uint32(r))
	}
	if caps.NumberInputValueCaps == 0 {
		return fmt.Errorf("touchpad: device declares no input values")
	}

	n := caps.NumberInputValueCaps
	valueCaps := make([]hidpValueCaps, n)
	length := n
	r, _, _ = procHidPGetValueCaps.Call(
		hidPInput,
		uintptr(unsafe.Pointer(&valueCaps[0])),
		uintptr(unsafe.Pointer(&length)),
		uintptr(unsafe.Pointer(&d.preparsed[0])),
	)
	if !ntStatusOK(r) {
		return fmt.Errorf("touchpad: HidP_GetValueCaps failed (status %#x)", uint32(r))
	}
	valueCaps = valueCaps[:length]

	// A finger collection is one that carries a Contact Identifier. X and Y
	// for that finger live in the same link collection.
	byCollection := map[uint16]*fingerSlot{}
	var order []uint16

	for i := range valueCaps {
		v := &valueCaps[i]
		if v.UsagePage == winapi.HID_USAGE_PAGE_DIGITIZER &&
			v.Usage == winapi.HID_USAGE_DIGITIZER_CONTACT_ID {
			if _, seen := byCollection[v.LinkCollection]; !seen {
				byCollection[v.LinkCollection] = &fingerSlot{collection: v.LinkCollection}
				order = append(order, v.LinkCollection)
			}
		}
		if v.UsagePage == winapi.HID_USAGE_PAGE_DIGITIZER &&
			v.Usage == winapi.HID_USAGE_DIGITIZER_CONTACT_CNT {
			d.countCollection = v.LinkCollection
			d.hasCount = true
		}
	}

	// Attach the coordinate ranges to their collections.
	for i := range valueCaps {
		v := &valueCaps[i]
		if v.UsagePage != winapi.HID_USAGE_PAGE_GENERIC {
			continue
		}
		slot, ok := byCollection[v.LinkCollection]
		if !ok {
			continue
		}
		switch v.Usage {
		case winapi.HID_USAGE_GENERIC_X:
			slot.xMin, slot.xMax, slot.hasX = v.LogicalMin, v.LogicalMax, true
		case winapi.HID_USAGE_GENERIC_Y:
			slot.yMin, slot.yMax, slot.hasY = v.LogicalMin, v.LogicalMax, true
		}
	}

	for _, c := range order {
		s := byCollection[c]
		// A slot without coordinates is useless for gestures.
		if s.hasX && s.hasY && s.xMax > s.xMin && s.yMax > s.yMin {
			d.slots = append(d.slots, *s)
		}
	}
	if len(d.slots) == 0 {
		return fmt.Errorf("touchpad: no usable finger collections in the report descriptor")
	}

	maxUsages, _, _ := procHidPMaxUsageListLength.Call(
		hidPInput, winapi.HID_USAGE_PAGE_DIGITIZER,
		uintptr(unsafe.Pointer(&d.preparsed[0])))
	if maxUsages < 8 {
		maxUsages = 8
	}
	d.usageBuf = make([]uint16, maxUsages)
	return nil
}

// Contact is one finger touching the pad, in normalised 0..1 pad coordinates.
type Contact struct {
	ID   int
	X, Y float64
}

// getValue reads a single HID usage value from a report.
func (d *Device) getValue(usagePage, collection, usage uint16, report []byte) (uint32, bool) {
	var val uint32
	r, _, _ := procHidPGetUsageValue.Call(
		hidPInput,
		uintptr(usagePage),
		uintptr(collection),
		uintptr(usage),
		uintptr(unsafe.Pointer(&val)),
		uintptr(unsafe.Pointer(&d.preparsed[0])),
		uintptr(unsafe.Pointer(&report[0])),
		uintptr(len(report)),
	)
	return val, ntStatusOK(r)
}

// tipDown reports whether the Tip Switch is set for a finger collection,
// which is how a Precision Touchpad says "this contact is actually touching".
func (d *Device) tipDown(collection uint16, report []byte) bool {
	length := uint32(len(d.usageBuf))
	r, _, _ := procHidPGetUsages.Call(
		hidPInput,
		winapi.HID_USAGE_PAGE_DIGITIZER,
		uintptr(collection),
		uintptr(unsafe.Pointer(&d.usageBuf[0])),
		uintptr(unsafe.Pointer(&length)),
		uintptr(unsafe.Pointer(&d.preparsed[0])),
		uintptr(unsafe.Pointer(&report[0])),
		uintptr(len(report)),
	)
	if !ntStatusOK(r) {
		return false
	}
	for i := uint32(0); i < length && int(i) < len(d.usageBuf); i++ {
		if d.usageBuf[i] == winapi.HID_USAGE_DIGITIZER_TIP_SWITCH {
			return true
		}
	}
	return false
}

// norm maps a logical coordinate into 0..1.
func norm(v uint32, lo, hi int32) float64 {
	span := float64(hi) - float64(lo)
	if span <= 0 {
		return 0
	}
	f := (float64(int32(v)) - float64(lo)) / span
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return f
}

// DecodeReport extracts the contacts present in a single HID report.
//
// It returns the contacts whose tip switch is down and the report's declared
// contact count. Those two together disambiguate the three cases the HID spec
// allows:
//
//	count > 0                    first report of a frame; count is the total
//	count == 0, contacts present continuation of the previous frame
//	count == 0, no contacts      every finger has lifted
//
// ok reports whether this was a touch report at all. It must stay true for
// the all-lifted case: that report is what ends a gesture and completes a
// tap, so discarding it strands the recogniser mid-gesture forever.
//
// A device multiplexes several report IDs onto one interface, so reports that
// are not touch reports do arrive here. Those are identified by the contact
// count field being unreadable — HidP_GetUsageValue returns
// HIDP_STATUS_INCOMPATIBLE_REPORT_ID — rather than by the report happening to
// carry no contacts.
func (d *Device) DecodeReport(report []byte, out []Contact) ([]Contact, int, bool) {
	if len(report) == 0 {
		return out[:0], 0, false
	}
	out = out[:0]

	count := 0
	countOK := false
	if d.hasCount {
		if v, ok := d.getValue(winapi.HID_USAGE_PAGE_DIGITIZER, d.countCollection,
			winapi.HID_USAGE_DIGITIZER_CONTACT_CNT, report); ok {
			count = int(v)
			countOK = true
		}
	}

	any := false
	for i := range d.slots {
		s := &d.slots[i]
		if !d.tipDown(s.collection, report) {
			continue
		}
		x, okX := d.getValue(winapi.HID_USAGE_PAGE_GENERIC, s.collection,
			winapi.HID_USAGE_GENERIC_X, report)
		y, okY := d.getValue(winapi.HID_USAGE_PAGE_GENERIC, s.collection,
			winapi.HID_USAGE_GENERIC_Y, report)
		if !okX || !okY {
			continue
		}
		id := i
		if v, ok := d.getValue(winapi.HID_USAGE_PAGE_DIGITIZER, s.collection,
			winapi.HID_USAGE_DIGITIZER_CONTACT_ID, report); ok {
			id = int(v)
		}
		out = append(out, Contact{
			ID: id,
			X:  norm(x, s.xMin, s.xMax),
			Y:  norm(y, s.yMin, s.yMax),
		})
		any = true
	}
	return out, count, countOK || any
}
