//go:build windows

package capture

import (
	"fmt"
	"math"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// DXGI Desktop Duplication capture.
//
// This is the modern, GPU-side capture path: it is what OBS's "Display
// Capture", most remote-desktop stacks and many recorders use. Verifying
// against it matters because it bypasses GDI entirely -- it reads the
// composited desktop surface straight from DWM. A window with
// WDA_EXCLUDEFROMCAPTURE is omitted from that surface, which is exactly the
// claim the verifier checks.
//
// The COM plumbing is hand-rolled to keep the project dependency-free.
// Interface pointers are held as unsafe.Pointer rather than uintptr so that
// no uintptr-to-pointer round trip is ever needed; the method indices below
// are vtable slots from d3d11.h / dxgi.h.

var (
	d3d11              = syscall.NewLazyDLL("d3d11.dll")
	procD3D11CreateDev = d3d11.NewProc("D3D11CreateDevice")
)

// vtable slot indices.
const (
	// IUnknown
	idxQueryInterface = 0
	idxRelease        = 2

	// IDXGIObject
	idxGetParent = 6

	// IDXGIDevice
	idxGetAdapter = 7

	// IDXGIAdapter
	idxEnumOutputs = 7

	// IDXGIOutput
	idxOutputGetDesc = 7
	// IDXGIOutput1
	idxDuplicateOutput = 22

	// IDXGIOutputDuplication
	idxAcquireNextFrame = 8
	idxReleaseFrame     = 14

	// ID3D11Device
	idxCreateTexture2D = 5

	// ID3D11DeviceContext
	idxMap          = 14
	idxUnmap        = 15
	idxCopyResource = 47

	// ID3D11Texture2D
	idxTextureGetDesc = 10
)

const (
	d3dDriverTypeHardware = 1
	d3d11SDKVersion       = 7

	d3d11UsageStaging  = 3
	d3d11CPUAccessRead = 0x20000
	d3d11MapRead       = 1

	dxgiErrorWaitTimeout = 0x887A0027
	dxgiErrorAccessLost  = 0x887A0026
	dxgiErrorNotFound    = 0x887A0002
	dxgiErrorUnsupported = 0x887A0004
	eAccessDenied        = 0x80070005
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIDXGIDevice   = guid{0x54ec77fa, 0x1377, 0x44e6, [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidIDXGIAdapter  = guid{0x2411e7e1, 0x12ac, 0x4ccf, [8]byte{0xbd, 0x14, 0x97, 0x98, 0xe8, 0x53, 0x4d, 0xc0}}
	iidIDXGIOutput1  = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// comCall invokes vtable slot idx on a COM object.
//
// Callers keep any Go structs whose addresses appear in args alive across the
// call with runtime.KeepAlive. Go's collector does not move heap objects, so
// passing their addresses to native code is sound.
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	// A nil interface (a COM call that "succeeded" but left an out-pointer
	// unset, which DXGI duplication can do transiently when hammered) must
	// surface as a failed HRESULT, never a nil dereference.
	if obj == nil {
		return uintptr(uint32(0x80004003)) // E_POINTER
	}
	vtbl := *(**[64]uintptr)(obj)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return r
}

func release(obj *unsafe.Pointer) {
	if *obj != nil {
		comCall(*obj, idxRelease)
		*obj = nil
	}
}

func hresultErr(op string, hr uintptr) error {
	switch uint32(hr) {
	case dxgiErrorUnsupported:
		return fmt.Errorf("capture: %s: DXGI_ERROR_UNSUPPORTED (duplication unavailable on this adapter/session)", op)
	case eAccessDenied:
		return fmt.Errorf("capture: %s: E_ACCESSDENIED (a secure desktop or another exclusive capture is active)", op)
	case dxgiErrorNotFound:
		return fmt.Errorf("capture: %s: DXGI_ERROR_NOT_FOUND", op)
	case dxgiErrorAccessLost:
		return fmt.Errorf("capture: %s: DXGI_ERROR_ACCESS_LOST", op)
	}
	return fmt.Errorf("capture: %s: HRESULT 0x%08X", op, uint32(hr))
}

// ---------------------------------------------------------------- structs

// Rect32 is a Win32 RECT.
type Rect32 struct{ Left, Top, Right, Bottom int32 }

type dxgiOutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates Rect32
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

type dxgiOutduplFrameInfo struct {
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerPosition           struct {
		X, Y    int32
		Visible int32
	}
	_                       int32 // padding to 8-byte alignment
	TotalMetadataBufferSize uint32
	PointerShapeBufferSize  uint32
}

type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

type d3d11MappedSubresource struct {
	PData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// ---------------------------------------------------------------- Duplicator

// Duplicator holds an open Desktop Duplication session.
type Duplicator struct {
	device  unsafe.Pointer
	context unsafe.Pointer
	dupl    unsafe.Pointer
	staging unsafe.Pointer

	// Bounds is the captured output's rectangle in virtual-desktop coordinates.
	Bounds Rect32
	W, H   int
}

// NewDuplicator opens Desktop Duplication for the output containing (px, py)
// in virtual-desktop coordinates, falling back to the first output.
func NewDuplicator(px, py int) (*Duplicator, error) {
	var device, context unsafe.Pointer
	hr, _, _ := procD3D11CreateDev.Call(
		0,                     // pAdapter (use the default adapter)
		d3dDriverTypeHardware, // DriverType
		0,                     // Software
		0,                     // Flags
		0,                     // pFeatureLevels
		0,                     // FeatureLevels
		d3d11SDKVersion,
		uintptr(unsafe.Pointer(&device)),
		0, // pFeatureLevel
		uintptr(unsafe.Pointer(&context)),
	)
	if hr != 0 || device == nil {
		return nil, hresultErr("D3D11CreateDevice", hr)
	}

	d := &Duplicator{device: device, context: context}
	if err := d.open(px, py); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *Duplicator) open(px, py int) error {
	var dxgiDev unsafe.Pointer
	if hr := comCall(d.device, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidIDXGIDevice)), uintptr(unsafe.Pointer(&dxgiDev))); hr != 0 {
		return hresultErr("QueryInterface(IDXGIDevice)", hr)
	}
	defer release(&dxgiDev)

	var adapter unsafe.Pointer
	if hr := comCall(dxgiDev, idxGetParent,
		uintptr(unsafe.Pointer(&iidIDXGIAdapter)), uintptr(unsafe.Pointer(&adapter))); hr != 0 {
		return hresultErr("GetParent(IDXGIAdapter)", hr)
	}
	defer release(&adapter)

	// Pick the output whose desktop rectangle contains the point.
	var chosen unsafe.Pointer
	var chosenDesc dxgiOutputDesc
	for i := uint32(0); i < 16; i++ {
		var out unsafe.Pointer
		if hr := comCall(adapter, idxEnumOutputs, uintptr(i), uintptr(unsafe.Pointer(&out))); hr != 0 {
			break
		}
		var desc dxgiOutputDesc
		hr := comCall(out, idxOutputGetDesc, uintptr(unsafe.Pointer(&desc)))
		runtime.KeepAlive(&desc)
		if hr != 0 {
			release(&out)
			continue
		}

		contains := int32(px) >= desc.DesktopCoordinates.Left && int32(px) < desc.DesktopCoordinates.Right &&
			int32(py) >= desc.DesktopCoordinates.Top && int32(py) < desc.DesktopCoordinates.Bottom

		if chosen == nil || contains {
			if chosen != nil {
				release(&chosen)
			}
			chosen, chosenDesc = out, desc
			if contains {
				break
			}
			continue
		}
		release(&out)
	}
	if chosen == nil {
		return fmt.Errorf("capture: no DXGI outputs found")
	}
	defer release(&chosen)

	var output1 unsafe.Pointer
	if hr := comCall(chosen, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidIDXGIOutput1)), uintptr(unsafe.Pointer(&output1))); hr != 0 {
		return hresultErr("QueryInterface(IDXGIOutput1)", hr)
	}
	defer release(&output1)

	var dupl unsafe.Pointer
	if hr := comCall(output1, idxDuplicateOutput,
		uintptr(d.device), uintptr(unsafe.Pointer(&dupl))); hr != 0 {
		return hresultErr("DuplicateOutput", hr)
	}

	d.dupl = dupl
	d.Bounds = chosenDesc.DesktopCoordinates
	d.W = int(chosenDesc.DesktopCoordinates.Right - chosenDesc.DesktopCoordinates.Left)
	d.H = int(chosenDesc.DesktopCoordinates.Bottom - chosenDesc.DesktopCoordinates.Top)
	return nil
}

// Close releases every COM object held by the duplicator.
func (d *Duplicator) Close() {
	release(&d.staging)
	release(&d.dupl)
	release(&d.context)
	release(&d.device)
}

// Grab acquires one desktop frame and returns it as BGRA.
//
// Desktop Duplication only produces a frame when the desktop actually
// changes, so this retries until the timeout elapses.
func (d *Duplicator) Grab(timeout time.Duration) (Image, error) {
	deadline := time.Now().Add(timeout)

	for {
		var info dxgiOutduplFrameInfo
		var resource unsafe.Pointer

		hr := comCall(d.dupl, idxAcquireNextFrame, 100,
			uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&resource)))
		runtime.KeepAlive(&info)

		if uint32(hr) == dxgiErrorWaitTimeout {
			if time.Now().After(deadline) {
				return Image{}, fmt.Errorf("capture: no desktop frame within %s", timeout)
			}
			continue
		}
		if hr != 0 {
			return Image{}, hresultErr("AcquireNextFrame", hr)
		}

		img, err := d.readResource(resource)
		release(&resource)
		comCall(d.dupl, idxReleaseFrame)

		if err != nil {
			return Image{}, err
		}
		d.normalizeBounds(img.W, img.H)

		// The first frame after DuplicateOutput can be a stale pre-existing
		// surface with no accumulated updates; prefer a genuinely fresh one.
		if info.AccumulatedFrames == 0 && time.Now().Before(deadline) {
			continue
		}
		return img, nil
	}
}

// normalizeBounds reconciles the output description with reality.
//
// Desktop Duplication always hands back *physical* pixels, but
// IDXGIOutput::GetDesc reports DesktopCoordinates in the calling process's
// coordinate space. A process that is not per-monitor DPI aware therefore
// sees scaled coordinates -- on a 1920x1200 display at 125% it reads
// 1536x960 -- and any attempt to map a screen coordinate into the captured
// image would land in the wrong place.
//
// Rescaling the origin by the observed ratio keeps the mapping correct
// regardless of the process's DPI awareness.
func (d *Duplicator) normalizeBounds(w, h int) {
	if w <= 0 || h <= 0 || (d.W == w && d.H == h) {
		return
	}
	if d.W > 0 && d.H > 0 {
		d.Bounds.Left = int32(math.Round(float64(d.Bounds.Left) * float64(w) / float64(d.W)))
		d.Bounds.Top = int32(math.Round(float64(d.Bounds.Top) * float64(h) / float64(d.H)))
	}
	d.W, d.H = w, h
	d.Bounds.Right = d.Bounds.Left + int32(w)
	d.Bounds.Bottom = d.Bounds.Top + int32(h)
}

// readResource copies the GPU texture behind a desktop resource into memory.
func (d *Duplicator) readResource(resource unsafe.Pointer) (Image, error) {
	var tex unsafe.Pointer
	if hr := comCall(resource, idxQueryInterface,
		uintptr(unsafe.Pointer(&iidID3D11Texture)), uintptr(unsafe.Pointer(&tex))); hr != 0 {
		return Image{}, hresultErr("QueryInterface(ID3D11Texture2D)", hr)
	}
	defer release(&tex)
	return textureToImage(d.device, d.context, tex, &d.staging)
}

// textureToImage copies a GPU texture through a CPU-readable staging texture
// and returns it as BGRA. The staging texture is cached across calls via the
// staging pointer, which the caller owns and must release.
//
// Both the Desktop Duplication and Windows.Graphics.Capture back ends end up
// holding an ID3D11Texture2D, so they share this final step.
func textureToImage(device, context, tex unsafe.Pointer, staging *unsafe.Pointer) (Image, error) {
	var desc d3d11Texture2DDesc
	comCall(tex, idxTextureGetDesc, uintptr(unsafe.Pointer(&desc)))
	runtime.KeepAlive(&desc)
	if desc.Width == 0 || desc.Height == 0 {
		return Image{}, fmt.Errorf("capture: desktop texture has zero size")
	}

	if *staging == nil {
		sd := desc
		sd.Usage = d3d11UsageStaging
		sd.BindFlags = 0
		sd.CPUAccessFlags = d3d11CPUAccessRead
		sd.MiscFlags = 0
		sd.MipLevels = 1
		sd.ArraySize = 1

		var st unsafe.Pointer
		hr := comCall(device, idxCreateTexture2D,
			uintptr(unsafe.Pointer(&sd)), 0, uintptr(unsafe.Pointer(&st)))
		runtime.KeepAlive(&sd)
		if hr != 0 {
			return Image{}, hresultErr("CreateTexture2D(staging)", hr)
		}
		*staging = st
	}

	comCall(context, idxCopyResource, uintptr(*staging), uintptr(tex))

	var mapped d3d11MappedSubresource
	if hr := comCall(context, idxMap, uintptr(*staging), 0, d3d11MapRead, 0,
		uintptr(unsafe.Pointer(&mapped))); hr != 0 {
		return Image{}, hresultErr("Map(staging)", hr)
	}
	defer comCall(context, idxUnmap, uintptr(*staging), 0)
	runtime.KeepAlive(&mapped)

	if mapped.PData == nil {
		return Image{}, fmt.Errorf("capture: Map returned a nil pointer")
	}

	w, h := int(desc.Width), int(desc.Height)
	pitch := int(mapped.RowPitch)
	if pitch < w*4 {
		return Image{}, fmt.Errorf("capture: unexpected row pitch %d for width %d", pitch, w)
	}
	out := make([]byte, w*h*4)
	src := unsafe.Slice((*byte)(mapped.PData), pitch*h)
	for y := 0; y < h; y++ {
		copy(out[y*w*4:(y+1)*w*4], src[y*pitch:y*pitch+w*4])
	}
	return Image{Pix: out, W: w, H: h}, nil
}

// DXGIScreen is a one-shot convenience wrapper around Duplicator.
func DXGIScreen(px, py int, timeout time.Duration) (Image, Rect32, error) {
	d, err := NewDuplicator(px, py)
	if err != nil {
		return Image{}, Rect32{}, err
	}
	defer d.Close()
	img, err := d.Grab(timeout)
	return img, d.Bounds, err
}
