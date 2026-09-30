// See ooidec.h. Compiled by cgo with -O3.
#include "ooidec.h"

#include <errno.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <vpx/vp8dx.h>
#include <vpx/vpx_decoder.h>

#include <dav1d/dav1d.h>

#include <wels/codec_api.h>

struct ooi_decoder {
	int codec;

	vpx_codec_ctx_t vpx;
	int vpx_open;

	Dav1dContext *dav1d;
	Dav1dPicture pic;
	int have_pic;

	ISVCDecoder *h264;
};

static int default_threads(int t) {
	if (t > 0) return t > 8 ? 8 : t;
	return 4;
}

ooi_decoder *ooi_open(int codec, int threads) {
	ooi_decoder *d = (ooi_decoder *)calloc(1, sizeof *d);
	if (!d) return NULL;
	d->codec = codec;
	threads = default_threads(threads);

	switch (codec) {
	case OOI_CODEC_VP8:
	case OOI_CODEC_VP9: {
		vpx_codec_dec_cfg_t cfg;
		memset(&cfg, 0, sizeof cfg);
		cfg.threads = (unsigned)threads;
		vpx_codec_iface_t *iface = codec == OOI_CODEC_VP8 ? vpx_codec_vp8_dx() : vpx_codec_vp9_dx();
		if (vpx_codec_dec_init(&d->vpx, iface, &cfg, 0) != VPX_CODEC_OK) goto fail;
		d->vpx_open = 1;
		if (codec == OOI_CODEC_VP9) {
			// Row-based multithreading parallelises within one frame, which is
			// what matters at 5-10 fps: latency per frame, not throughput.
			vpx_codec_control(&d->vpx, VP9D_SET_ROW_MT, 1);
		}
		break;
	}
	case OOI_CODEC_AV1: {
		Dav1dSettings s;
		dav1d_default_settings(&s);
		s.n_threads = threads;
		// Without this dav1d buffers several frames for frame-parallel decoding,
		// adding latency that is pointless at low frame rates.
		s.max_frame_delay = 1;
		if (dav1d_open(&d->dav1d, &s) < 0) goto fail;
		break;
	}
	case OOI_CODEC_H264: {
		if (WelsCreateDecoder(&d->h264) != 0 || !d->h264) goto fail;
		SDecodingParam p;
		memset(&p, 0, sizeof p);
		p.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_AVC;
		// No error concealment: a concealed frame is a smeared frame. The Go side
		// freezes on the last good picture and asks for a keyframe instead.
		p.eEcActiveIdc = ERROR_CON_DISABLE;
		if ((*d->h264)->Initialize(d->h264, &p) != 0) goto fail;
		break;
	}
	default:
		goto fail;
	}
	return d;

fail:
	ooi_close(d);
	return NULL;
}

void ooi_close(ooi_decoder *d) {
	if (!d) return;
	if (d->vpx_open) vpx_codec_destroy(&d->vpx);
	if (d->have_pic) dav1d_picture_unref(&d->pic);
	if (d->dav1d) dav1d_close(&d->dav1d);
	if (d->h264) {
		(*d->h264)->Uninitialize(d->h264);
		WelsDestroyDecoder(d->h264);
	}
	free(d);
}

static int decode_vpx(ooi_decoder *d, const uint8_t *data, size_t len, ooi_picture *out) {
	if (vpx_codec_decode(&d->vpx, data, (unsigned int)len, NULL, 0) != VPX_CODEC_OK)
		return OOI_ERR;

	// A frame decoded against a missing reference is marked corrupt. Showing it
	// would paint artefacts, so report it as a failure.
	int corrupted = 0;
	if (vpx_codec_control(&d->vpx, VP8D_GET_FRAME_CORRUPTED, &corrupted) == VPX_CODEC_OK && corrupted)
		return OOI_ERR;

	vpx_codec_iter_t it = NULL;
	vpx_image_t *img, *last = NULL;
	while ((img = vpx_codec_get_frame(&d->vpx, &it)) != NULL) last = img;
	if (!last) return OOI_NO_PICTURE;
	if (last->fmt != VPX_IMG_FMT_I420) return OOI_ERR_FORMAT;

	out->y = last->planes[VPX_PLANE_Y];
	out->u = last->planes[VPX_PLANE_U];
	out->v = last->planes[VPX_PLANE_V];
	out->y_stride = last->stride[VPX_PLANE_Y];
	out->uv_stride = last->stride[VPX_PLANE_U];
	out->width = (int)last->d_w;
	out->height = (int)last->d_h;
	out->matrix = last->cs == VPX_CS_BT_709 ? OOI_MATRIX_BT709 : OOI_MATRIX_BT601;
	out->range = last->range == VPX_CR_FULL_RANGE ? OOI_RANGE_FULL : OOI_RANGE_LIMITED;
	return OOI_OK;
}

static void keep_dav1d_picture(ooi_decoder *d, Dav1dPicture *p) {
	if (d->have_pic) dav1d_picture_unref(&d->pic);
	d->pic = *p;
	d->have_pic = 1;
}

static int decode_av1(ooi_decoder *d, const uint8_t *data, size_t len, ooi_picture *out) {
	Dav1dData dd;
	memset(&dd, 0, sizeof dd);
	uint8_t *buf = dav1d_data_create(&dd, len);
	if (!buf) return OOI_ERR;
	memcpy(buf, data, len);

	int got = 0;
	// Bounded: send_data and get_picture alternate until the unit is consumed.
	for (int guard = 0; dd.sz > 0 && guard < 64; guard++) {
		int res = dav1d_send_data(d->dav1d, &dd);
		if (res < 0 && res != DAV1D_ERR(EAGAIN)) {
			dav1d_data_unref(&dd);
			return OOI_ERR;
		}
		Dav1dPicture p;
		memset(&p, 0, sizeof p);
		int r2 = dav1d_get_picture(d->dav1d, &p);
		if (r2 == 0) {
			keep_dav1d_picture(d, &p);
			got = 1;
		} else if (r2 != DAV1D_ERR(EAGAIN)) {
			dav1d_data_unref(&dd);
			return OOI_ERR;
		}
	}
	if (dd.sz > 0) dav1d_data_unref(&dd);

	if (!got) {
		Dav1dPicture p;
		memset(&p, 0, sizeof p);
		if (dav1d_get_picture(d->dav1d, &p) == 0) {
			keep_dav1d_picture(d, &p);
			got = 1;
		}
	}
	if (!got) return OOI_NO_PICTURE;

	if (d->pic.p.layout != DAV1D_PIXEL_LAYOUT_I420 || d->pic.p.bpc != 8) return OOI_ERR_FORMAT;

	out->y = (const uint8_t *)d->pic.data[0];
	out->u = (const uint8_t *)d->pic.data[1];
	out->v = (const uint8_t *)d->pic.data[2];
	out->y_stride = (int)d->pic.stride[0];
	out->uv_stride = (int)d->pic.stride[1];
	out->width = d->pic.p.w;
	out->height = d->pic.p.h;
	out->matrix = OOI_MATRIX_BT601;
	out->range = OOI_RANGE_LIMITED;
	if (d->pic.seq_hdr) {
		if (d->pic.seq_hdr->mtrx == DAV1D_MC_BT709) out->matrix = OOI_MATRIX_BT709;
		if (d->pic.seq_hdr->color_range) out->range = OOI_RANGE_FULL;
	}
	return OOI_OK;
}

static int decode_h264(ooi_decoder *d, const uint8_t *data, size_t len, ooi_picture *out) {
	unsigned char *dst[3] = {0, 0, 0};
	SBufferInfo info;
	memset(&info, 0, sizeof info);
	DECODING_STATE st = (*d->h264)->DecodeFrameNoDelay(d->h264, data, (int)len, dst, &info);
	// Pending is benign; anything else (lost reference, bitstream error, no
	// parameter sets, concealment) means this picture must not be shown.
	if (st & ~dsFramePending) return OOI_ERR;
	if (info.iBufferStatus != 1 || !dst[0]) return OOI_NO_PICTURE;

	out->y = dst[0];
	out->u = dst[1];
	out->v = dst[2];
	out->y_stride = info.UsrData.sSystemBuffer.iStride[0];
	out->uv_stride = info.UsrData.sSystemBuffer.iStride[1];
	out->width = info.UsrData.sSystemBuffer.iWidth;
	out->height = info.UsrData.sSystemBuffer.iHeight;
	out->matrix = OOI_MATRIX_BT601;
	out->range = OOI_RANGE_LIMITED;
	return OOI_OK;
}

int ooi_decode(ooi_decoder *d, const uint8_t *data, size_t len, ooi_picture *out) {
	if (!d || !data || !len || !out) return OOI_ERR;
	switch (d->codec) {
	case OOI_CODEC_VP8:
	case OOI_CODEC_VP9: return decode_vpx(d, data, len, out);
	case OOI_CODEC_AV1: return decode_av1(d, data, len, out);
	case OOI_CODEC_H264: return decode_h264(d, data, len, out);
	}
	return OOI_ERR;
}

// ---------------------------------------------------------------- resampling
//
// Separable antialiased tent filter, the same construction Pillow uses for its
// BILINEAR resize. The kernel's support widens with the shrink factor, so each
// output pixel is a weighted average over its whole source footprint. That is
// what keeps small text legible when a 1080p screen is shown in a smaller
// overlay; a plain bilinear or nearest filter drops pixels and aliases.

#define PREC 14 // fixed-point weight precision

typedef struct {
	int *start; // first contributing source index per output index
	int *count; // number of contributors
	int32_t *w; // count[i] weights per output, stride = ksize
	int ksize;
} coeffs;

static void coeffs_free(coeffs *c) {
	free(c->start);
	free(c->count);
	free(c->w);
}

static double tent(double x) {
	if (x < 0) x = -x;
	return x < 1.0 ? 1.0 - x : 0.0;
}

static int coeffs_build(coeffs *c, int in, int out) {
	memset(c, 0, sizeof *c);
	double scale = (double)in / (double)out;
	double fscale = scale < 1.0 ? 1.0 : scale;
	double support = 1.0 * fscale;
	int ksize = (int)ceil(support) * 2 + 1;

	c->start = (int *)malloc(sizeof(int) * (size_t)out);
	c->count = (int *)malloc(sizeof(int) * (size_t)out);
	c->w = (int32_t *)calloc((size_t)out * (size_t)ksize, sizeof(int32_t));
	double *tmp = (double *)malloc(sizeof(double) * (size_t)ksize);
	if (!c->start || !c->count || !c->w || !tmp) {
		free(tmp);
		coeffs_free(c);
		return -1;
	}
	c->ksize = ksize;

	for (int i = 0; i < out; i++) {
		double center = ((double)i + 0.5) * scale;
		int xmin = (int)(center - support + 0.5);
		int xmax = (int)(center + support + 0.5);
		if (xmin < 0) xmin = 0;
		if (xmax > in) xmax = in;
		int n = xmax - xmin;
		if (n > ksize) n = ksize;
		if (n < 1) { // degenerate: pin to nearest valid sample
			n = 1;
			if (xmin >= in) xmin = in - 1;
		}

		double sum = 0;
		for (int k = 0; k < n; k++) {
			double w = tent(((double)(xmin + k) - center + 0.5) / fscale);
			tmp[k] = w;
			sum += w;
		}
		// Normalise so the weights sum to exactly 1<<PREC; the rounding residue
		// goes to the largest tap so flat areas reproduce exactly.
		int32_t *wi = c->w + (size_t)i * (size_t)ksize;
		int32_t acc = 0, big = 0;
		for (int k = 0; k < n; k++) {
			double v = sum > 0 ? tmp[k] / sum : (k == 0 ? 1.0 : 0.0);
			wi[k] = (int32_t)(v * (1 << PREC) + 0.5);
			acc += wi[k];
			if (wi[k] > wi[big]) big = k;
		}
		wi[big] += (1 << PREC) - acc;

		c->start[i] = xmin;
		c->count[i] = n;
	}
	free(tmp);
	return 0;
}

static inline uint8_t clamp8(int32_t v) {
	return (uint8_t)(v < 0 ? 0 : (v > 255 ? 255 : v));
}

// resample_plane scales one 8-bit plane from (sw x sh, sstride) to dw x dh,
// writing a tightly packed dw-stride result.
static int resample_plane(const uint8_t *src, int sstride, int sw, int sh, uint8_t *dst, int dw, int dh) {
	if (sw == dw && sh == dh) {
		for (int y = 0; y < dh; y++) memcpy(dst + (size_t)y * dw, src + (size_t)y * sstride, (size_t)dw);
		return 0;
	}

	coeffs cx, cy;
	if (coeffs_build(&cx, sw, dw) < 0) return -1;
	if (coeffs_build(&cy, sh, dh) < 0) {
		coeffs_free(&cx);
		return -1;
	}

	// Horizontal pass into a dw x sh intermediate.
	uint8_t *mid = (uint8_t *)malloc((size_t)dw * (size_t)sh);
	if (!mid) {
		coeffs_free(&cx);
		coeffs_free(&cy);
		return -1;
	}
	const int32_t half = 1 << (PREC - 1);
	for (int y = 0; y < sh; y++) {
		const uint8_t *row = src + (size_t)y * sstride;
		uint8_t *mrow = mid + (size_t)y * dw;
		for (int x = 0; x < dw; x++) {
			const int32_t *w = cx.w + (size_t)x * cx.ksize;
			const uint8_t *s = row + cx.start[x];
			int32_t acc = half;
			for (int k = 0; k < cx.count[x]; k++) acc += (int32_t)s[k] * w[k];
			mrow[x] = clamp8(acc >> PREC);
		}
	}

	// Vertical pass.
	for (int y = 0; y < dh; y++) {
		const int32_t *w = cy.w + (size_t)y * cy.ksize;
		int y0 = cy.start[y], n = cy.count[y];
		uint8_t *drow = dst + (size_t)y * dw;
		for (int x = 0; x < dw; x++) {
			int32_t acc = half;
			const uint8_t *s = mid + (size_t)y0 * dw + x;
			for (int k = 0; k < n; k++) acc += (int32_t)s[(size_t)k * dw] * w[k];
			drow[x] = clamp8(acc >> PREC);
		}
	}

	free(mid);
	coeffs_free(&cx);
	coeffs_free(&cy);
	return 0;
}

// Integer YUV -> RGB coefficients, scaled by 256.
typedef struct {
	int yoff, yk, rv, gu, gv, bu;
} yuvmat;

static const yuvmat MATS[2][2] = {
	// [matrix][range]
	{/* BT.601 */ {16, 298, 409, 100, 208, 516}, {0, 256, 359, 88, 183, 454}},
	{/* BT.709 */ {16, 298, 459, 55, 136, 541}, {0, 256, 403, 48, 120, 475}},
};

int ooi_render(const ooi_picture *pic, uint8_t *canvas, int cw, int ch, int fx, int fy, int fw, int fh) {
	if (!pic || !canvas || cw <= 0 || ch <= 0 || fw <= 0 || fh <= 0) return -1;
	if (fx < 0 || fy < 0 || fx + fw > cw || fy + fh > ch) return -1;
	if (!pic->y || !pic->u || !pic->v || pic->width <= 0 || pic->height <= 0) return -1;

	size_t n = (size_t)fw * (size_t)fh;
	uint8_t *ys = (uint8_t *)malloc(n), *us = (uint8_t *)malloc(n), *vs = (uint8_t *)malloc(n);
	if (!ys || !us || !vs) {
		free(ys);
		free(us);
		free(vs);
		return -1;
	}

	int cwid = (pic->width + 1) / 2, chei = (pic->height + 1) / 2;
	int rc = 0;
	rc |= resample_plane(pic->y, pic->y_stride, pic->width, pic->height, ys, fw, fh);
	rc |= resample_plane(pic->u, pic->uv_stride, cwid, chei, us, fw, fh);
	rc |= resample_plane(pic->v, pic->uv_stride, cwid, chei, vs, fw, fh);
	if (rc) {
		free(ys);
		free(us);
		free(vs);
		return -1;
	}

	// Transparent letterbox: zero is fully transparent under premultiplied alpha.
	memset(canvas, 0, (size_t)cw * (size_t)ch * 4);

	int mi = pic->matrix == OOI_MATRIX_BT709 ? 1 : 0;
	int ri = pic->range == OOI_RANGE_FULL ? 1 : 0;
	const yuvmat m = MATS[mi][ri];

	for (int y = 0; y < fh; y++) {
		uint8_t *out = canvas + ((size_t)(fy + y) * cw + fx) * 4;
		const uint8_t *yr = ys + (size_t)y * fw, *ur = us + (size_t)y * fw, *vr = vs + (size_t)y * fw;
		for (int x = 0; x < fw; x++) {
			int32_t c = ((int32_t)yr[x] - m.yoff) * m.yk;
			int32_t dd = (int32_t)ur[x] - 128;
			int32_t e = (int32_t)vr[x] - 128;
			out[0] = clamp8((c + m.bu * dd + 128) >> 8);
			out[1] = clamp8((c - m.gu * dd - m.gv * e + 128) >> 8);
			out[2] = clamp8((c + m.rv * e + 128) >> 8);
			out[3] = 0xFF; // opaque, so premultiplied == straight
			out += 4;
		}
	}

	free(ys);
	free(us);
	free(vs);
	return 0;
}

const char *ooi_version(void) {
	static char buf[160];
	OpenH264Version v = WelsGetCodecVersion();
	snprintf(buf, sizeof buf, "libvpx %s, dav1d %s, openh264 %u.%u.%u", vpx_codec_version_str(), dav1d_version(),
	         v.uMajor, v.uMinor, v.uRevision);
	return buf;
}
