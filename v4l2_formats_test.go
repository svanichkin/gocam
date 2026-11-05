package gocam

import (
	"bytes"
	"math/rand"
	"testing"
)

// The V4L2 conversion as it was: the full frame converted, then sampled.

// convertFrame normalizes a single captured frame from various V4L2 pixel
// formats into a tightly packed YCbCr 4:4:4 buffer (packed Y, Cb, Cr per pixel).
func convertFrameReference(src []byte, pixFmt uint32, width, height, stride int) []byte {
	if width <= 0 || height <= 0 {
		return nil
	}

	// All output frames are packed YCbCr 4:4:4: 3 bytes per pixel.
	dstSize := width * height * 3
	if dstSize <= 0 {
		return nil
	}
	dst := make([]byte, dstSize)

	switch pixFmt {
	case pixFmtYUV24:
		// Source is already packed YUV444 (Y, Cb, Cr) but may have stride.
		rowBytes := width * 3
		if rowBytes <= 0 || len(src) < rowBytes {
			return nil
		}

		effectiveStride := stride
		if effectiveStride <= 0 {
			effectiveStride = rowBytes
		}
		if height > 0 && effectiveStride*height > len(src) {
			effectiveStride = len(src) / height
			if effectiveStride < rowBytes {
				return nil
			}
		}

		for y := 0; y < height; y++ {
			inStart := y * effectiveStride
			inEnd := inStart + rowBytes
			if inEnd > len(src) {
				return nil
			}
			row := src[inStart:inEnd]

			for x := 0; x < width; x++ {
				si := x * 3
				di := (y*width + x) * 3
				if si+2 >= len(row) || di+2 >= len(dst) {
					break
				}
				dst[di] = row[si]     // Y
				dst[di+1] = row[si+1] // Cb
				dst[di+2] = row[si+2] // Cr
			}
		}

	case pixFmtNV12:
		// NV12: Y plane (full res), then interleaved CbCr at 2x2 subsampling.
		rowBytesY := width
		if rowBytesY <= 0 || len(src) < rowBytesY {
			return nil
		}

		effectiveStride := stride
		if effectiveStride <= 0 {
			effectiveStride = rowBytesY
		}

		totalLines := height + height/2
		if effectiveStride*totalLines > len(src) {
			effectiveStride = len(src) / totalLines
			if effectiveStride < rowBytesY {
				return nil
			}
		}

		yPlaneSize := effectiveStride * height
		if yPlaneSize > len(src) {
			return nil
		}

		yPlane := src[:yPlaneSize]
		uvPlane := src[yPlaneSize:]

		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				// Luma from Y plane.
				yIdx := y*effectiveStride + x
				if yIdx >= len(yPlane) {
					return nil
				}
				Y := yPlane[yIdx]

				// Chroma from UV plane: 2x2 block.
				uvY := y / 2
				uvX := x / 2
				uvIdx := uvY*effectiveStride + uvX*2
				if uvIdx+1 >= len(uvPlane) {
					return nil
				}
				Cb := uvPlane[uvIdx]
				Cr := uvPlane[uvIdx+1]

				di := (y*width + x) * 3
				if di+2 >= len(dst) {
					return nil
				}
				dst[di] = Y
				dst[di+1] = Cb
				dst[di+2] = Cr
			}
		}

	case pixFmtYUYV:
		// YUYV 4:2:2: Y0 U Y1 V for each pair of pixels.
		rowBytes := width * 2
		if rowBytes <= 0 || len(src) < rowBytes {
			return nil
		}

		effectiveStride := stride
		if effectiveStride <= 0 {
			effectiveStride = rowBytes
		}
		if height > 0 && effectiveStride*height > len(src) {
			effectiveStride = len(src) / height
			if effectiveStride < rowBytes {
				return nil
			}
		}

		for y := 0; y < height; y++ {
			inStart := y * effectiveStride
			inEnd := inStart + rowBytes
			if inEnd > len(src) {
				return nil
			}
			row := src[inStart:inEnd]

			for x := 0; x < width; x += 2 {
				si := x * 2
				if si+3 >= len(row) {
					break
				}

				Y0 := row[si]
				U := row[si+1]
				Y1 := row[si+2]
				V := row[si+3]

				// First pixel
				di0 := (y*width + x) * 3
				if di0+2 < len(dst) {
					dst[di0] = Y0
					dst[di0+1] = U
					dst[di0+2] = V
				}

				// Second pixel (shares U,V)
				if x+1 < width {
					di1 := (y*width + x + 1) * 3
					if di1+2 < len(dst) {
						dst[di1] = Y1
						dst[di1+1] = U
						dst[di1+2] = V
					}
				}
			}
		}

	case pixFmtRGB24:
		// RGB24 -> YCbCr444
		rowBytes := width * 3
		if rowBytes <= 0 || len(src) < rowBytes {
			return nil
		}

		effectiveStride := stride
		if effectiveStride <= 0 {
			effectiveStride = rowBytes
		}
		if height > 0 && effectiveStride*height > len(src) {
			effectiveStride = len(src) / height
			if effectiveStride < rowBytes {
				return nil
			}
		}

		for y := 0; y < height; y++ {
			inStart := y * effectiveStride
			inEnd := inStart + rowBytes
			if inEnd > len(src) {
				return nil
			}
			row := src[inStart:inEnd]

			for x := 0; x < width; x++ {
				si := x * 3
				if si+2 >= len(row) {
					break
				}

				R := int(row[si])
				G := int(row[si+1])
				B := int(row[si+2])

				Y := (66*R+129*G+25*B+128)>>8 + 16
				Cb := (-38*R-74*G+112*B+128)>>8 + 128
				Cr := (112*R-94*G-18*B+128)>>8 + 128

				di := (y*width + x) * 3
				if di+2 >= len(dst) {
					return nil
				}
				dst[di] = clampToByte(Y)
				dst[di+1] = clampToByte(Cb)
				dst[di+2] = clampToByte(Cr)
			}
		}

	default:
		return nil
	}

	return dst
}

// resampleYCbCr444Fill downsamples a packed YCbCr444 buffer into dstW x dstH,
// preserving aspect ratio with a centered crop ("fill"). If the source is
// already smaller than or equal to dst in both dimensions, it returns the
// original buffer (no upscaling).
func resampleYCbCr444FillReference(src []byte, srcW, srcH, dstW, dstH int) []byte {
	if srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 {
		return nil
	}
	if len(src) < srcW*srcH*3 {
		return nil
	}

	// No upscaling: if source fits entirely within dst, keep it as-is.
	if srcW <= dstW && srcH <= dstH {
		return src
	}

	dst := make([]byte, dstW*dstH*3)

	// Compute centered crop region in source.
	srcAspect := float64(srcW) / float64(srcH)
	dstAspect := float64(dstW) / float64(dstH)

	cropW := srcW
	cropH := srcH
	srcX0 := 0
	srcY0 := 0

	if srcAspect > dstAspect {
		// Source is wider than destination: crop left/right.
		cropH = srcH
		cropW = int(float64(srcH) * dstAspect)
		if cropW > srcW {
			cropW = srcW
		}
		if cropW < 1 {
			cropW = 1
		}
		srcX0 = (srcW - cropW) / 2
		srcY0 = 0
	} else {
		// Source is taller than destination: crop top/bottom.
		cropW = srcW
		cropH = int(float64(srcW) / dstAspect)
		if cropH > srcH {
			cropH = srcH
		}
		if cropH < 1 {
			cropH = 1
		}
		srcX0 = 0
		srcY0 = (srcH - cropH) / 2
	}

	for dy := 0; dy < dstH; dy++ {
		syRel := 0
		if dstH > 0 {
			syRel = dy * cropH / dstH
		}
		sy := srcY0 + syRel
		if sy >= srcH {
			sy = srcH - 1
		}

		for dx := 0; dx < dstW; dx++ {
			sxRel := 0
			if dstW > 0 {
				sxRel = dx * cropW / dstW
			}
			sx := srcX0 + sxRel
			if sx >= srcW {
				sx = srcW - 1
			}

			srcIndex := (sy*srcW + sx) * 3
			dstIndex := (dy*dstW + dx) * 3
			if srcIndex+2 >= len(src) || dstIndex+2 >= len(dst) {
				continue
			}

			dst[dstIndex] = src[srcIndex]
			dst[dstIndex+1] = src[srcIndex+1]
			dst[dstIndex+2] = src[srcIndex+2]
		}
	}

	return dst
}

// The one-pass sampler must give exactly what conversion plus resampling
// gave, for the formats gocam supported before.
func TestSampleMatchesConvertThenResample(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	sizes := [][2]int{{352, 288}, {640, 480}, {1280, 720}, {320, 240}, {1920, 1080}, {176, 144}, {1080, 1920}, {642, 362}}
	for _, f := range []uint32{pixFmtYUV24, pixFmtNV12, pixFmtYUYV, pixFmtRGB24} {
		for _, sz := range sizes {
			w, h := sz[0], sz[1]
			for _, pad := range []int{0, 64} {
				stride := bytesPerLine(f, w) + pad
				src := make([]byte, frameBytes(f, stride, h))
				rng.Read(src)
				got, gw, gh := sampleYCbCr444(src, f, w, h, stride)
				full := convertFrameReference(src, f, w, h, stride)
				want := full
				ww, wh := w, h
				if w > cifWidth || h > cifHeight {
					want, ww, wh = resampleYCbCr444FillReference(full, w, h, cifWidth, cifHeight), cifWidth, cifHeight
				}
				if gw != ww || gh != wh || !bytes.Equal(got, want) {
					t.Fatalf("%s %dx%d stride %d: got %dx%d, differs from the reference", pixFmtName(f), w, h, stride, gw, gh)
				}
			}
		}
	}
}

// pack builds a frame of format f from a packed 4:4:4 image whose chroma is
// constant over each 2x2 block (so 4:2:x formats carry it exactly).
func pack(img []byte, w, h int, f uint32) []byte {
	px := func(x, y int) (byte, byte, byte) {
		i := (y*w + x) * 3
		return img[i], img[i+1], img[i+2]
	}
	stride := bytesPerLine(f, w)
	out := make([]byte, frameBytes(f, stride, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			Y, cb, cr := px(x, y)
			switch f {
			case pixFmtNV12, pixFmtNV21:
				out[y*stride+x] = Y
				if x%2 == 0 && y%2 == 0 {
					uv := stride*h + (y/2)*stride + x
					if f == pixFmtNV12 {
						out[uv], out[uv+1] = cb, cr
					} else {
						out[uv], out[uv+1] = cr, cb
					}
				}
			case pixFmtYUYV, pixFmtUYVY, pixFmtYVYU, pixFmtVYUY:
				p := y*stride + (x/2)*4
				var order [4]byte // bytes of the pair, by role
				Y0, _, _ := px(x&^1, y)
				Y1, _, _ := px(x|1, y)
				switch f {
				case pixFmtYUYV:
					order = [4]byte{Y0, cb, Y1, cr}
				case pixFmtUYVY:
					order = [4]byte{cb, Y0, cr, Y1}
				case pixFmtYVYU:
					order = [4]byte{Y0, cr, Y1, cb}
				case pixFmtVYUY:
					order = [4]byte{cr, Y0, cb, Y1}
				}
				copy(out[p:p+4], order[:])
			}
		}
	}
	return out
}

func TestSampleNewFormats(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, sz := range [][2]int{{352, 288}, {640, 480}, {1280, 720}} {
		w, h := sz[0], sz[1]
		img := make([]byte, w*h*3)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := (y*w + x) * 3
				img[i] = byte(rng.Intn(256))
				if x%2 == 0 && y%2 == 0 {
					img[i+1], img[i+2] = byte(rng.Intn(256)), byte(rng.Intn(256))
				} else {
					j := ((y&^1)*w + (x &^ 1)) * 3
					img[i+1], img[i+2] = img[j+1], img[j+2]
				}
			}
		}
		want, ww, wh := sampleYCbCr444(img, pixFmtYUV24, w, h, w*3)
		for _, f := range []uint32{pixFmtNV12, pixFmtNV21, pixFmtYUYV, pixFmtUYVY, pixFmtYVYU, pixFmtVYUY} {
			got, gw, gh := sampleYCbCr444(pack(img, w, h, f), f, w, h, bytesPerLine(f, w))
			if gw != ww || gh != wh || !bytes.Equal(got, want) {
				t.Fatalf("%s %dx%d differs from the 4:4:4 source", pixFmtName(f), w, h)
			}
		}
		// BGR is RGB with red and blue swapped.
		rgb := make([]byte, w*h*3)
		rng.Read(rgb)
		bgr := make([]byte, len(rgb))
		for i := 0; i < len(rgb); i += 3 {
			bgr[i], bgr[i+1], bgr[i+2] = rgb[i+2], rgb[i+1], rgb[i]
		}
		a, _, _ := sampleYCbCr444(rgb, pixFmtRGB24, w, h, w*3)
		b, _, _ := sampleYCbCr444(bgr, pixFmtBGR24, w, h, w*3)
		if !bytes.Equal(a, b) {
			t.Fatalf("BGR24 %dx%d differs from RGB24", w, h)
		}
	}
	if d, _, _ := sampleYCbCr444(make([]byte, 10), pixFmtYUYV, 352, 288, 704); d != nil {
		t.Fatal("a short buffer must be rejected")
	}
}

func TestFourccValues(t *testing.T) {
	// Values from linux/videodev2.h.
	for f, want := range map[uint32]uint32{pixFmtYUYV: 0x56595559, pixFmtNV12: 0x3231564E, pixFmtRGB24: 0x33424752, pixFmtYUV24: 0x33565559, pixFmtUYVY: 0x59565955} {
		if f != want {
			t.Fatalf("%s = 0x%x, want 0x%x", pixFmtName(f), f, want)
		}
	}
}
