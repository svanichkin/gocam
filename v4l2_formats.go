package gocam

// Pixel formats and conversion for the V4L2 backend. This file has no
// build tag so the conversion can be tested on any platform.

func fourcc(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

// V4L2 pixel formats gocam can convert, in order of preference: the ones
// closest to packed YCbCr 4:4:4 first.
var (
	pixFmtYUV24 = fourcc('Y', 'U', 'V', '3') // packed 4:4:4
	pixFmtNV12  = fourcc('N', 'V', '1', '2') // Y plane, then CbCr 2x2
	pixFmtNV21  = fourcc('N', 'V', '2', '1') // Y plane, then CrCb 2x2
	pixFmtYUYV  = fourcc('Y', 'U', 'Y', 'V') // 4:2:2 Y0 Cb Y1 Cr
	pixFmtUYVY  = fourcc('U', 'Y', 'V', 'Y') // 4:2:2 Cb Y0 Cr Y1 (PinePhone sensors)
	pixFmtYVYU  = fourcc('Y', 'V', 'Y', 'U') // 4:2:2 Y0 Cr Y1 Cb
	pixFmtVYUY  = fourcc('V', 'Y', 'U', 'Y') // 4:2:2 Cr Y0 Cb Y1
	pixFmtRGB24 = fourcc('R', 'G', 'B', '3')
	pixFmtBGR24 = fourcc('B', 'G', 'R', '3')
)

var preferredPixFormats = []uint32{
	pixFmtYUV24, pixFmtNV12, pixFmtNV21, pixFmtYUYV, pixFmtUYVY, pixFmtYVYU, pixFmtVYUY, pixFmtRGB24, pixFmtBGR24,
}

func pixFmtName(f uint32) string {
	b := []byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)}
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// bytesPerLine is the minimal stride of one row (of the Y plane for NV12/21).
func bytesPerLine(f uint32, width int) int {
	switch f {
	case pixFmtYUV24, pixFmtRGB24, pixFmtBGR24:
		return width * 3
	case pixFmtYUYV, pixFmtUYVY, pixFmtYVYU, pixFmtVYUY:
		return width * 2
	case pixFmtNV12, pixFmtNV21:
		return width
	}
	return 0
}

// frameBytes is the minimal size of a frame with the given stride.
func frameBytes(f uint32, stride, height int) int {
	switch f {
	case pixFmtNV12, pixFmtNV21:
		return stride * (height + (height+1)/2)
	}
	return stride * height
}

// Media bus formats (linux/media-bus-format.h) a sensor may put on its pad,
// and the pixel formats a capture node can store them in.
const (
	mbusRGB888_1X24 = 0x100a
	mbusUYVY8_2X8   = 0x2006
	mbusVYUY8_2X8   = 0x2007
	mbusYUYV8_2X8   = 0x2008
	mbusYVYU8_2X8   = 0x2009
	mbusUYVY8_1X16  = 0x200f
	mbusVYUY8_1X16  = 0x2010
	mbusYUYV8_1X16  = 0x2011
	mbusYVYU8_1X16  = 0x2012
)

// pixFormatsForMbus returns the pixel formats worth trying on a capture node
// fed by a sensor with this bus format: the matching packed format first,
// then the planar ones a bridge (like the A64 CSI) can convert to.
func pixFormatsForMbus(code uint32) []uint32 {
	var packed uint32
	switch code {
	case mbusUYVY8_2X8, mbusUYVY8_1X16:
		packed = pixFmtUYVY
	case mbusVYUY8_2X8, mbusVYUY8_1X16:
		packed = pixFmtVYUY
	case mbusYUYV8_2X8, mbusYUYV8_1X16:
		packed = pixFmtYUYV
	case mbusYVYU8_2X8, mbusYVYU8_1X16:
		packed = pixFmtYVYU
	case mbusRGB888_1X24:
		return []uint32{pixFmtRGB24, pixFmtBGR24}
	default:
		return nil
	}
	return []uint32{packed, pixFmtNV12, pixFmtNV21, pixFmtYUYV, pixFmtUYVY}
}

// isYUVOrRGBMbus reports whether gocam can use a sensor bus format as is
// (raw Bayer, for one, would need an ISP).
func isYUVOrRGBMbus(code uint32) bool {
	return pixFormatsForMbus(code) != nil
}

const (
	cifWidth  = 352
	cifHeight = 288
)

// outputSize is the size gocam delivers for a source frame: CIF when the
// source is larger in either dimension, otherwise the source size (no
// upscaling).
func outputSize(w, h int) (int, int) {
	if w > cifWidth || h > cifHeight {
		return cifWidth, cifHeight
	}
	return w, h
}

// cropRect is the centred region of a w x h source with the aspect of
// dstW x dstH ("fill"); the whole source when it is not downscaled.
func cropRect(w, h, dstW, dstH int) (x0, y0, cw, ch int) {
	if w <= dstW && h <= dstH {
		return 0, 0, w, h
	}
	srcAspect := float64(w) / float64(h)
	dstAspect := float64(dstW) / float64(dstH)
	cw, ch = w, h
	if srcAspect > dstAspect {
		cw = min(max(int(float64(h)*dstAspect), 1), w)
		x0 = (w - cw) / 2
	} else {
		ch = min(max(int(float64(w)/dstAspect), 1), h)
		y0 = (h - ch) / 2
	}
	return x0, y0, cw, ch
}

// sampleYCbCr444 converts a captured frame of any supported format into
// packed YCbCr 4:4:4 at outputSize, centre-cropped to the CIF aspect when
// downscaled. It reads only the sampled source pixels, in one pass: no full
// size intermediate frame. Returns nil if src is too short or the format is
// unknown.
func sampleYCbCr444(src []byte, f uint32, w, h, stride int) ([]byte, int, int) {
	if w <= 0 || h <= 0 {
		return nil, 0, 0
	}
	if min := bytesPerLine(f, w); min == 0 {
		return nil, 0, 0
	} else if stride < min {
		stride = min
	}
	if need := frameBytes(f, stride, h); len(src) < need {
		// Some drivers report a stride larger than what they fill.
		stride = bytesPerLine(f, w)
		if len(src) < frameBytes(f, stride, h) {
			return nil, 0, 0
		}
	}
	outW, outH := outputSize(w, h)
	x0, y0, cw, ch := cropRect(w, h, outW, outH)

	cols := make([]int, outW)
	for dx := range cols {
		cols[dx] = min(x0+dx*cw/outW, w-1)
	}
	dst := make([]byte, outW*outH*3)
	di := 0
	for dy := 0; dy < outH; dy++ {
		sy := min(y0+dy*ch/outH, h-1)
		row := src[sy*stride:]
		switch f {
		case pixFmtYUV24:
			for _, sx := range cols {
				dst[di], dst[di+1], dst[di+2] = row[sx*3], row[sx*3+1], row[sx*3+2]
				di += 3
			}
		case pixFmtNV12, pixFmtNV21:
			uv := src[stride*h+(sy/2)*stride:]
			cb, cr := 0, 1
			if f == pixFmtNV21 {
				cb, cr = 1, 0
			}
			for _, sx := range cols {
				u := (sx / 2) * 2
				dst[di], dst[di+1], dst[di+2] = row[sx], uv[u+cb], uv[u+cr]
				di += 3
			}
		case pixFmtYUYV, pixFmtUYVY, pixFmtYVYU, pixFmtVYUY:
			// Byte offsets of Y0, Y1, Cb, Cr within a 4-byte pair.
			var y0o, y1o, cbo, cro int
			switch f {
			case pixFmtYUYV:
				y0o, cbo, y1o, cro = 0, 1, 2, 3
			case pixFmtUYVY:
				cbo, y0o, cro, y1o = 0, 1, 2, 3
			case pixFmtYVYU:
				y0o, cro, y1o, cbo = 0, 1, 2, 3
			case pixFmtVYUY:
				cro, y0o, cbo, y1o = 0, 1, 2, 3
			}
			for _, sx := range cols {
				p := (sx / 2) * 4
				yo := y0o
				if sx&1 == 1 {
					yo = y1o
				}
				dst[di], dst[di+1], dst[di+2] = row[p+yo], row[p+cbo], row[p+cro]
				di += 3
			}
		case pixFmtRGB24, pixFmtBGR24:
			ro, bo := 0, 2
			if f == pixFmtBGR24 {
				ro, bo = 2, 0
			}
			for _, sx := range cols {
				r, g, b := int(row[sx*3+ro]), int(row[sx*3+1]), int(row[sx*3+bo])
				dst[di] = clampToByte((66*r+129*g+25*b+128)>>8 + 16)
				dst[di+1] = clampToByte((-38*r-74*g+112*b+128)>>8 + 128)
				dst[di+2] = clampToByte((112*r-94*g-18*b+128)>>8 + 128)
				di += 3
			}
		default:
			return nil, 0, 0
		}
	}
	return dst, outW, outH
}

func clampToByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
