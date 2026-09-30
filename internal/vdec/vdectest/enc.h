#ifndef VDECTEST_ENC_H
#define VDECTEST_ENC_H
#include <stddef.h>
#include <stdint.h>

// vt_encode encodes n synthetic frames with the given codec (1=VP8, 2=VP9,
// 4=H264) into a malloc'd buffer of records: [u32 len][u8 keyframe][len bytes].
// It returns the number of frames, or < 0 on failure. Free *out with free().
int vt_encode(int codec, int w, int h, int n, uint8_t **out, size_t *outlen);

#endif
