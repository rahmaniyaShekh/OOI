// ooidec: a small C facade over libvpx (VP8/VP9), dav1d (AV1) and openh264
// (H.264), plus a high-quality I420 -> premultiplied BGRA resampler.
//
// Everything that touches pixels lives here, compiled with -O3, so the Go side
// only moves compressed frames and pointers.
#ifndef OOIDEC_H
#define OOIDEC_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

enum {
	OOI_CODEC_VP8 = 1,
	OOI_CODEC_VP9 = 2,
	OOI_CODEC_AV1 = 3,
	OOI_CODEC_H264 = 4,
};

// Colour matrix and range, as signalled by the bitstream.
enum { OOI_MATRIX_BT601 = 0, OOI_MATRIX_BT709 = 1 };
enum { OOI_RANGE_LIMITED = 0, OOI_RANGE_FULL = 1 };

// Result codes.
enum {
	OOI_OK = 0,          // a new picture is ready
	OOI_NO_PICTURE = 1,  // input accepted, no picture yet (not an error)
	OOI_ERR = -1,        // decode failed: the stream is broken until a keyframe
	OOI_ERR_FORMAT = -2, // decoded, but not 8-bit 4:2:0 (unsupported)
};

// A decoded 8-bit 4:2:0 picture. The planes stay valid until the next decode
// call on the same decoder, or until it is closed.
typedef struct {
	const uint8_t *y, *u, *v;
	int y_stride, uv_stride;
	int width, height;
	int matrix, range;
} ooi_picture;

typedef struct ooi_decoder ooi_decoder;

// ooi_open creates a decoder. threads <= 0 picks a sensible default.
ooi_decoder *ooi_open(int codec, int threads);

// ooi_decode feeds one complete compressed frame (a temporal unit / access unit).
// On OOI_OK, *out describes the newest picture.
int ooi_decode(ooi_decoder *d, const uint8_t *data, size_t len, ooi_picture *out);

void ooi_close(ooi_decoder *d);

// ooi_render resamples pic into a canvas of canvas_w x canvas_h BGRA pixels,
// placing the image at the letterbox rectangle (fx, fy, fw, fh). Everything
// outside that rectangle is written as zero, which is fully transparent under
// premultiplied alpha. The filter is an antialiased separable tent: a true
// area-weighted average when shrinking (crisp, alias-free text) and bilinear
// when enlarging. Chroma is upsampled by the same filter, so there is no
// separate nearest-neighbour chroma step.
//
// Returns 0 on success, -1 on bad arguments or allocation failure.
int ooi_render(const ooi_picture *pic, uint8_t *canvas, int canvas_w, int canvas_h,
               int fx, int fy, int fw, int fh);

// ooi_version reports the linked library versions, for `ooi version`.
const char *ooi_version(void);

#ifdef __cplusplus
}
#endif

#endif
