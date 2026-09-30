// Test-only encoders. They produce real bitstreams so the decoders, keyframe
// detection and colour pipeline are exercised against genuine codec output.
#include "enc.h"

#include <stdlib.h>
#include <string.h>

#include <vpx/vp8cx.h>
#include <vpx/vpx_encoder.h>
#include <wels/codec_api.h>

// fill paints a deterministic test pattern: flat grey field, a saturated box
// that moves one step per frame, and fine vertical stripes (text-like detail).
static void fill(uint8_t *y, int ys, uint8_t *u, uint8_t *v, int uvs, int w, int h, int t) {
	for (int j = 0; j < h; j++)
		for (int i = 0; i < w; i++) y[j * ys + i] = 126;
	for (int j = 0; j < (h + 1) / 2; j++)
		for (int i = 0; i < (w + 1) / 2; i++) {
			u[j * uvs + i] = 128;
			v[j * uvs + i] = 128;
		}
	// Moving BT.601 red box.
	int bx = (t * 4) % (w / 2), by = h / 4, bw = w / 4, bh = h / 4;
	for (int j = by; j < by + bh && j < h; j++)
		for (int i = bx; i < bx + bw && i < w; i++) y[j * ys + i] = 81;
	for (int j = by / 2; j < (by + bh) / 2; j++)
		for (int i = bx / 2; i < (bx + bw) / 2; i++) {
			u[j * uvs + i] = 90;
			v[j * uvs + i] = 240;
		}
	// Stripes in the bottom band.
	for (int j = h * 3 / 4; j < h; j++)
		for (int i = 0; i < w; i++) y[j * ys + i] = (i & 1) ? 235 : 16;
}

typedef struct {
	uint8_t *buf;
	size_t len, cap;
} sink;

static int put(sink *s, const void *p, size_t n) {
	if (s->len + n > s->cap) {
		size_t nc = s->cap ? s->cap * 2 : 1 << 16;
		while (nc < s->len + n) nc *= 2;
		uint8_t *nb = (uint8_t *)realloc(s->buf, nc);
		if (!nb) return -1;
		s->buf = nb;
		s->cap = nc;
	}
	memcpy(s->buf + s->len, p, n);
	s->len += n;
	return 0;
}

static int put_frame(sink *s, const uint8_t *data, uint32_t n, uint8_t key) {
	if (put(s, &n, 4) || put(s, &key, 1) || put(s, data, n)) return -1;
	return 0;
}

static int enc_vpx(int vp9, int w, int h, int n, sink *s) {
	vpx_codec_iface_t *iface = vp9 ? vpx_codec_vp9_cx() : vpx_codec_vp8_cx();
	vpx_codec_enc_cfg_t cfg;
	if (vpx_codec_enc_config_default(iface, &cfg, 0)) return -1;
	cfg.g_w = (unsigned)w;
	cfg.g_h = (unsigned)h;
	cfg.g_timebase.num = 1;
	cfg.g_timebase.den = 10;
	cfg.rc_target_bitrate = 3000;
	cfg.g_lag_in_frames = 0;
	// Periodic keyframes so a peer joining mid-stream (or looping the canned
	// sequence) resynchronises within a second or two.
	cfg.kf_mode = VPX_KF_AUTO;
	cfg.kf_max_dist = 20;
	vpx_codec_ctx_t enc;
	if (vpx_codec_enc_init(&enc, iface, &cfg, 0)) return -1;
	vpx_codec_control(&enc, VP8E_SET_CPUUSED, 8);

	vpx_image_t img;
	if (!vpx_img_alloc(&img, VPX_IMG_FMT_I420, (unsigned)w, (unsigned)h, 16)) {
		vpx_codec_destroy(&enc);
		return -1;
	}
	int frames = 0;
	for (int t = 0; t < n; t++) {
		fill(img.planes[0], img.stride[0], img.planes[1], img.planes[2], img.stride[1], w, h, t);
		if (vpx_codec_encode(&enc, &img, t, 1, t == 0 ? VPX_EFLAG_FORCE_KF : 0, VPX_DL_REALTIME)) break;
		vpx_codec_iter_t it = NULL;
		const vpx_codec_cx_pkt_t *pkt;
		while ((pkt = vpx_codec_get_cx_data(&enc, &it)) != NULL) {
			if (pkt->kind != VPX_CODEC_CX_FRAME_PKT) continue;
			put_frame(s, (const uint8_t *)pkt->data.frame.buf, (uint32_t)pkt->data.frame.sz,
			          (pkt->data.frame.flags & VPX_FRAME_IS_KEY) ? 1 : 0);
			frames++;
		}
	}
	vpx_img_free(&img);
	vpx_codec_destroy(&enc);
	return frames;
}

static int enc_h264(int w, int h, int n, sink *s) {
	ISVCEncoder *enc = NULL;
	if (WelsCreateSVCEncoder(&enc) || !enc) return -1;
	SEncParamBase p;
	memset(&p, 0, sizeof p);
	p.iUsageType = SCREEN_CONTENT_REAL_TIME;
	p.fMaxFrameRate = 10;
	p.iPicWidth = w;
	p.iPicHeight = h;
	p.iTargetBitrate = 3000000;
	if ((*enc)->Initialize(enc, &p)) {
		WelsDestroySVCEncoder(enc);
		return -1;
	}
	int ys = w, uvs = (w + 1) / 2;
	uint8_t *y = (uint8_t *)malloc((size_t)ys * h);
	uint8_t *u = (uint8_t *)malloc((size_t)uvs * ((h + 1) / 2));
	uint8_t *v = (uint8_t *)malloc((size_t)uvs * ((h + 1) / 2));
	int frames = 0;
	for (int t = 0; t < n; t++) {
		fill(y, ys, u, v, uvs, w, h, t);
		SSourcePicture pic;
		memset(&pic, 0, sizeof pic);
		pic.iPicWidth = w;
		pic.iPicHeight = h;
		pic.iColorFormat = videoFormatI420;
		pic.iStride[0] = ys;
		pic.iStride[1] = pic.iStride[2] = uvs;
		pic.pData[0] = y;
		pic.pData[1] = u;
		pic.pData[2] = v;
		pic.uiTimeStamp = (long long)t * 100;
		SFrameBSInfo info;
		memset(&info, 0, sizeof info);
		if ((*enc)->EncodeFrame(enc, &pic, &info) != cmResultSuccess) break;
		if (info.eFrameType == videoFrameTypeSkip) continue;
		// Concatenate every NAL of every layer: one access unit per frame.
		sink au = {0};
		for (int l = 0; l < info.iLayerNum; l++) {
			SLayerBSInfo *L = &info.sLayerInfo[l];
			int sz = 0;
			for (int k = 0; k < L->iNalCount; k++) sz += L->pNalLengthInByte[k];
			put(&au, L->pBsBuf, (size_t)sz);
		}
		put_frame(s, au.buf, (uint32_t)au.len, info.eFrameType == videoFrameTypeIDR ? 1 : 0);
		free(au.buf);
		frames++;
	}
	free(y);
	free(u);
	free(v);
	(*enc)->Uninitialize(enc);
	WelsDestroySVCEncoder(enc);
	return frames;
}

int vt_encode(int codec, int w, int h, int n, uint8_t **out, size_t *outlen) {
	sink s = {0};
	int frames = -1;
	if (codec == 1) frames = enc_vpx(0, w, h, n, &s);
	if (codec == 2) frames = enc_vpx(1, w, h, n, &s);
	if (codec == 4) frames = enc_h264(w, h, n, &s);
	*out = s.buf;
	*outlen = s.len;
	return frames;
}
