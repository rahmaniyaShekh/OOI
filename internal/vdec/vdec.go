// Package vdec decodes WebRTC video (AV1, VP9, VP8, H.264) and resamples it
// into the overlay's premultiplied BGRA surface.
//
// Decoding and all per-pixel work happen in C (dav1d, libvpx, openh264 and a
// separable antialiased resampler), linked statically so ooi.exe has no DLLs.
package vdec

/*
#cgo CFLAGS: -O3 -I${SRCDIR}/../../third_party/x/mingw64/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/x/mingw64/lib -l:libvpx.a -l:libdav1d.a -l:libopenh264.a -static -static-libgcc -static-libstdc++ -lstdc++ -lpthread -lm
#include <stdlib.h>
#include "ooidec.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"ooi/internal/frame"
)

// Codec identifies a video codec.
type Codec int

// Supported codecs, in the C enum's numbering.
const (
	VP8  Codec = C.OOI_CODEC_VP8
	VP9  Codec = C.OOI_CODEC_VP9
	AV1  Codec = C.OOI_CODEC_AV1
	H264 Codec = C.OOI_CODEC_H264
)

func (c Codec) String() string {
	switch c {
	case VP8:
		return "VP8"
	case VP9:
		return "VP9"
	case AV1:
		return "AV1"
	case H264:
		return "H264"
	}
	return fmt.Sprintf("codec(%d)", int(c))
}

// CodecFromMime maps a WebRTC mime type such as "video/VP9" to a Codec.
func CodecFromMime(mime string) (Codec, bool) {
	switch strings.ToLower(mime) {
	case "video/vp8":
		return VP8, true
	case "video/vp9":
		return VP9, true
	case "video/av1":
		return AV1, true
	case "video/h264":
		return H264, true
	}
	return 0, false
}

var (
	// ErrCorrupt means this frame could not be decoded cleanly. The stream is
	// unusable until the next keyframe.
	ErrCorrupt = errors.New("vdec: frame could not be decoded cleanly")
	// ErrFormat means the stream is not 8-bit 4:2:0.
	ErrFormat = errors.New("vdec: unsupported pixel format")
)

// Decoder wraps one C decoder. It is not safe for concurrent use.
type Decoder struct {
	d     *C.ooi_decoder
	codec Codec
	pic   C.ooi_picture
	has   bool
}

// New opens a decoder. threads <= 0 picks a default.
func New(c Codec, threads int) (*Decoder, error) {
	d := C.ooi_open(C.int(c), C.int(threads))
	if d == nil {
		return nil, fmt.Errorf("vdec: cannot open %s decoder", c)
	}
	return &Decoder{d: d, codec: c}, nil
}

// Codec reports what this decoder handles.
func (d *Decoder) Codec() Codec { return d.codec }

// Decode feeds one complete compressed frame. It reports whether a new picture
// is now available; that picture stays valid until the next Decode or Close.
func (d *Decoder) Decode(b []byte) (bool, error) {
	if d.d == nil {
		return false, errors.New("vdec: decoder closed")
	}
	if len(b) == 0 {
		return false, nil
	}
	rc := C.ooi_decode(d.d, (*C.uint8_t)(unsafe.Pointer(&b[0])), C.size_t(len(b)), &d.pic)
	switch rc {
	case C.OOI_OK:
		d.has = true
		return true, nil
	case C.OOI_NO_PICTURE:
		return false, nil
	case C.OOI_ERR_FORMAT:
		d.has = false
		return false, ErrFormat
	default:
		d.has = false
		return false, ErrCorrupt
	}
}

// Size reports the current picture's dimensions.
func (d *Decoder) Size() (int, int) {
	if !d.has {
		return 0, 0
	}
	return int(d.pic.width), int(d.pic.height)
}

// Render resamples the current picture into a new canvas-sized BGRA image,
// letterboxed. The result is ready to hand to the overlay.
func (d *Decoder) Render(cw, ch int) (*frame.Prepared, frame.Rect, error) {
	if !d.has {
		return nil, frame.Rect{}, errors.New("vdec: no picture")
	}
	if cw <= 0 || ch <= 0 {
		return nil, frame.Rect{}, errors.New("vdec: empty canvas")
	}
	fit := frame.Fit(int(d.pic.width), int(d.pic.height), cw, ch)
	if fit.W == 0 || fit.H == 0 {
		return nil, frame.Rect{}, errors.New("vdec: nothing to draw")
	}
	out := &frame.Prepared{Pix: make([]byte, cw*ch*4), W: cw, H: ch}
	rc := C.ooi_render(&d.pic, (*C.uint8_t)(unsafe.Pointer(&out.Pix[0])), C.int(cw), C.int(ch),
		C.int(fit.X), C.int(fit.Y), C.int(fit.W), C.int(fit.H))
	if rc != 0 {
		return nil, frame.Rect{}, errors.New("vdec: render failed")
	}
	return out, fit, nil
}

// Close releases the decoder.
func (d *Decoder) Close() {
	if d.d != nil {
		C.ooi_close(d.d)
		d.d = nil
		d.has = false
	}
}

// Version reports the linked decoder library versions.
func Version() string { return C.GoString(C.ooi_version()) }

// Picture is a plain Go description of an I420 picture, used to drive the
// resampler directly (tests, and anything that already has raw planes).
type Picture struct {
	Y, U, V          []byte
	YStride          int
	UVStride         int
	Width, Height    int
	BT709, FullRange bool
}

// RenderPicture resamples a caller-supplied picture. It exists so the colour
// and filter maths can be tested without an encoded stream.
func RenderPicture(p Picture, cw, ch int) (*frame.Prepared, frame.Rect, error) {
	if len(p.Y) == 0 || len(p.U) == 0 || len(p.V) == 0 {
		return nil, frame.Rect{}, errors.New("vdec: empty planes")
	}
	fit := frame.Fit(p.Width, p.Height, cw, ch)
	if fit.W == 0 || fit.H == 0 {
		return nil, frame.Rect{}, errors.New("vdec: nothing to draw")
	}
	// The planes are copied into C memory so the C side never holds Go pointers
	// inside a C struct (which cgo's pointer rules forbid).
	y := C.CBytes(p.Y)
	u := C.CBytes(p.U)
	v := C.CBytes(p.V)
	defer C.free(y)
	defer C.free(u)
	defer C.free(v)

	var pic C.ooi_picture
	pic.y = (*C.uint8_t)(y)
	pic.u = (*C.uint8_t)(u)
	pic.v = (*C.uint8_t)(v)
	pic.y_stride = C.int(p.YStride)
	pic.uv_stride = C.int(p.UVStride)
	pic.width = C.int(p.Width)
	pic.height = C.int(p.Height)
	if p.BT709 {
		pic.matrix = C.OOI_MATRIX_BT709
	}
	if p.FullRange {
		pic._range = C.OOI_RANGE_FULL
	}

	out := &frame.Prepared{Pix: make([]byte, cw*ch*4), W: cw, H: ch}
	rc := C.ooi_render(&pic, (*C.uint8_t)(unsafe.Pointer(&out.Pix[0])), C.int(cw), C.int(ch),
		C.int(fit.X), C.int(fit.Y), C.int(fit.W), C.int(fit.H))
	if rc != 0 {
		return nil, frame.Rect{}, errors.New("vdec: render failed")
	}
	return out, fit, nil
}
