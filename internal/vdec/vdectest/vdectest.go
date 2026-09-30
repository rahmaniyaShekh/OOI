// Package vdectest produces real encoded video for tests. It is only imported
// by tests, so the encoders it links never reach ooi.exe.
package vdectest

/*
#cgo CFLAGS: -O2 -I${SRCDIR}/../../../third_party/x/mingw64/include
#cgo LDFLAGS: -L${SRCDIR}/../../../third_party/x/mingw64/lib -l:libvpx.a -l:libopenh264.a -lstdc++ -lpthread -lm
#include <stdlib.h>
#include "enc.h"
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// Frame is one encoded frame and whether the encoder marked it a keyframe.
type Frame struct {
	Data []byte
	Key  bool
}

// Codec numbers match vdec's.
const (
	VP8  = 1
	VP9  = 2
	H264 = 4
)

// Encode produces n frames of a synthetic moving pattern.
func Encode(codec, w, h, n int) ([]Frame, error) {
	var out *C.uint8_t
	var outLen C.size_t
	got := C.vt_encode(C.int(codec), C.int(w), C.int(h), C.int(n), &out, &outLen)
	if out != nil {
		defer C.free(unsafe.Pointer(out))
	}
	if got < 0 {
		return nil, fmt.Errorf("vdectest: encoder %d failed", codec)
	}
	raw := C.GoBytes(unsafe.Pointer(out), C.int(outLen))
	var frames []Frame
	for len(raw) >= 5 {
		n := int(binary.LittleEndian.Uint32(raw))
		if 5+n > len(raw) {
			return nil, fmt.Errorf("vdectest: truncated record")
		}
		frames = append(frames, Frame{Data: raw[5 : 5+n], Key: raw[4] == 1})
		raw = raw[5+n:]
	}
	return frames, nil
}
