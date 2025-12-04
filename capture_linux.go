//go:build linux
// +build linux

package gocam

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	v4l2BufTypeVideoCapture = 1
	v4l2FieldAny            = 0
	v4l2MemoryMMap          = 1
)

const (
	v4l2PixFmtRGB24 = 0x33424752 // 'RGB3'
	v4l2PixFmtYUYV  = 0x56595559 // 'YUYV'
	v4l2PixFmtNV12  = 0x3231564E // 'NV12'
	v4l2PixFmtYUV24 = 0x33565559 // 'YUV3' (packed 4:4:4, 8 bits per component)
)

const (
	v4l2CapVideoCapture = 0x00000001
	v4l2CapStreaming    = 0x04000000
	v4l2CapDeviceCaps   = 0x80000000
)

const (
	cifWidth  = 352
	cifHeight = 288
)

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	Pixelformat  uint32
	Field        uint32
	Bytesperline uint32
	Sizeimage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

type v4l2Format struct {
	Type uint32
	_    [4]byte // align union to 64-bit boundary like C's struct v4l2_format
	fmt  [200]byte
}

type v4l2RequestBuffers struct {
	Count    uint32
	Type     uint32
	Memory   uint32
	Reserved [2]uint32
}

type v4l2Timecode struct {
	Type     uint32
	Flags    uint32
	Frames   uint8
	Seconds  uint8
	Minutes  uint8
	Hours    uint8
	Userbits [4]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	Bytesused uint32
	Flags     uint32
	Field     uint32
	Timestamp syscall.Timeval
	Timecode  v4l2Timecode
	Sequence  uint32
	Memory    uint32
	Offset    uint32
	_         uint32 // union padding
	Length    uint32
	Reserved2 uint32
	Reserved  uint32
}

const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14
	iocDirBits  = 2

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift)
}

func io(typ, nr uintptr) uintptr {
	return ioc(iocNone, typ, nr, 0)
}

func iow(typ, nr, size uintptr) uintptr {
	return ioc(iocWrite, typ, nr, size)
}

func ior(typ, nr, size uintptr) uintptr {
	return ioc(iocRead, typ, nr, size)
}

func iowr(typ, nr, size uintptr) uintptr {
	return ioc(iocRead|iocWrite, typ, nr, size)
}

var (
	vidiocQuerycap  = ior(uintptr('V'), 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocSFmt      = iowr(uintptr('V'), 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs   = iowr(uintptr('V'), 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQuerybuf  = iowr(uintptr('V'), 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf      = iowr(uintptr('V'), 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf     = iowr(uintptr('V'), 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn  = iow(uintptr('V'), 18, unsafe.Sizeof(uint32(0)))
	vidiocStreamOff = iow(uintptr('V'), 19, unsafe.Sizeof(uint32(0)))
)

type mappedBuffer struct {
	data   []byte
	length uint32
}

var camLog = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

func v4l2CString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// logCameraConfig prints a human-readable description of the current camera configuration.
func logCameraConfig(caps *v4l2Capability, pixelFormat uint32, width, height, stride int) {
	if width <= 0 || height <= 0 {
		return
	}

	driver := v4l2CString(caps.Driver[:])
	card := v4l2CString(caps.Card[:])
	bus := v4l2CString(caps.BusInfo[:])

	formatIn := "UNKNOWN"
	switch pixelFormat {
	case v4l2PixFmtYUV24:
		formatIn = "YUV24 (YCbCr 4:4:4)"
	case v4l2PixFmtNV12:
		formatIn = "NV12 (YCbCr 4:2:0)"
	case v4l2PixFmtYUYV:
		formatIn = "YUYV (YCbCr 4:2:2)"
	case v4l2PixFmtRGB24:
		formatIn = "RGB24"
	}

	bufPixels := width * height
	bufBytes := bufPixels * 3

	camLog.Println("[gocam] [V4L2]")
	camLog.Printf("[gocam]   /dev/video0 (Capture)\n")
	if card != "" || driver != "" || bus != "" {
		camLog.Printf("[gocam]     Card:       %s\n", card)
		camLog.Printf("[gocam]     Driver:     %s\n", driver)
		camLog.Printf("[gocam]     Bus:        %s\n", bus)
	}
	camLog.Printf("[gocam]     Format:      %s -> YCbCr 4:4:4 (uint8)\n", formatIn)
	camLog.Printf("[gocam]     Resolution:  %d x %d\n", width, height)
	camLog.Printf("[gocam]     Stride:      %d bytes\n", stride)
	camLog.Printf("[gocam]     Buffer:      %d*3 (%d bytes)\n", bufPixels, bufBytes)
	camLog.Println("[gocam]     Conversion:")
	camLog.Println("[gocam]       Pre Format Conversion:  NO (device native)")
	camLog.Println("[gocam]       Post Format Conversion: YES (to packed YCbCr444)")
	camLog.Println("[gocam]       Resampling:             NO")
}

// StartStream opens /dev/video0, configures a V4L2 capture stream, and
// returns a channel of frames encoded as tightly packed YCbCr 4:4:4 (YUV24) buffers.
func StartStream(ctx context.Context) (<-chan Frame, error) {
	fd, err := syscall.Open("/dev/video0", syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("gocam: cannot open /dev/video0: %w", err)
	}

	var (
		buffers       []mappedBuffer
		streamStarted bool
	)

	cleanup := func() {
		if streamStarted {
			bufType := uint32(v4l2BufTypeVideoCapture)
			_ = ioctl(fd, vidiocStreamOff, unsafe.Pointer(&bufType))
		}
		for _, mb := range buffers {
			if mb.data != nil {
				_ = syscall.Munmap(mb.data)
			}
		}
		_ = syscall.Close(fd)
	}

	var caps v4l2Capability
	if err := ioctl(fd, vidiocQuerycap, unsafe.Pointer(&caps)); err != nil {
		cleanup()
		return nil, fmt.Errorf("gocam: VIDIOC_QUERYCAP failed: %w", err)
	}

	capsToCheck := caps.Capabilities
	if capsToCheck&v4l2CapDeviceCaps != 0 {
		capsToCheck = caps.DeviceCaps
	}
	if capsToCheck&v4l2CapVideoCapture == 0 {
		cleanup()
		return nil, fmt.Errorf("gocam: device does not support video capture")
	}
	if capsToCheck&v4l2CapStreaming == 0 {
		cleanup()
		return nil, fmt.Errorf("gocam: device does not support streaming I/O")
	}

	const (
		defaultWidth  = cifWidth
		defaultHeight = cifHeight
	)

	width := uint32(defaultWidth)
	height := uint32(defaultHeight)

	pixelFormat := uint32(v4l2PixFmtYUV24)

	format := v4l2Format{Type: v4l2BufTypeVideoCapture}
	pix := (*v4l2PixFormat)(unsafe.Pointer(&format.fmt[0]))
	pix.Width = width
	pix.Height = height
	pix.Pixelformat = pixelFormat
	pix.Field = v4l2FieldAny

	if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
		cleanup()
		return nil, fmt.Errorf("gocam: VIDIOC_S_FMT YUV24 failed: %w", err)
	}

	pixelFormat = pix.Pixelformat
	width = pix.Width
	height = pix.Height
	stride := int(pix.Bytesperline)

	if pixelFormat != v4l2PixFmtYUV24 {
		// Fallback to NV12
		pixelFormat = v4l2PixFmtNV12

		format = v4l2Format{Type: v4l2BufTypeVideoCapture}
		pix = (*v4l2PixFormat)(unsafe.Pointer(&format.fmt[0]))
		pix.Width = width
		pix.Height = height
		pix.Pixelformat = pixelFormat
		pix.Field = v4l2FieldAny

		if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
			cleanup()
			return nil, fmt.Errorf("gocam: VIDIOC_S_FMT fallback NV12 failed: %w", err)
		}

		pixelFormat = pix.Pixelformat
		width = pix.Width
		height = pix.Height
		stride = int(pix.Bytesperline)

		if pixelFormat != v4l2PixFmtNV12 {
			// Fallback to YUYV
			pixelFormat = v4l2PixFmtYUYV

			format = v4l2Format{Type: v4l2BufTypeVideoCapture}
			pix = (*v4l2PixFormat)(unsafe.Pointer(&format.fmt[0]))
			pix.Width = width
			pix.Height = height
			pix.Pixelformat = pixelFormat
			pix.Field = v4l2FieldAny

			if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
				cleanup()
				return nil, fmt.Errorf("gocam: VIDIOC_S_FMT fallback YUYV failed: %w", err)
			}

			pixelFormat = pix.Pixelformat
			width = pix.Width
			height = pix.Height
			stride = int(pix.Bytesperline)

			if pixelFormat != v4l2PixFmtYUYV {
				// Final fallback to RGB24
				pixelFormat = v4l2PixFmtRGB24

				format = v4l2Format{Type: v4l2BufTypeVideoCapture}
				pix = (*v4l2PixFormat)(unsafe.Pointer(&format.fmt[0]))
				pix.Width = width
				pix.Height = height
				pix.Pixelformat = pixelFormat
				pix.Field = v4l2FieldAny

				if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
					cleanup()
					return nil, fmt.Errorf("gocam: VIDIOC_S_FMT fallback RGB24 failed: %w", err)
				}

				pixelFormat = pix.Pixelformat
				width = pix.Width
				height = pix.Height
				stride = int(pix.Bytesperline)
				if pixelFormat != v4l2PixFmtRGB24 {
					cleanup()
					return nil, fmt.Errorf("gocam: unsupported pixel format 0x%x", pixelFormat)
				}
			}
		}
	}

	if stride == 0 {
		switch pixelFormat {
		case v4l2PixFmtRGB24:
			stride = int(width) * 3
		case v4l2PixFmtYUYV:
			stride = int(width) * 2
		case v4l2PixFmtNV12:
			stride = int(width)
		case v4l2PixFmtYUV24:
			stride = int(width) * 3
		}
	}

	req := v4l2RequestBuffers{
		Count:  4,
		Type:   v4l2BufTypeVideoCapture,
		Memory: v4l2MemoryMMap,
	}
	if err := ioctl(fd, vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		cleanup()
		return nil, fmt.Errorf("gocam: VIDIOC_REQBUFS failed: %w", err)
	}
	if req.Count < 2 {
		cleanup()
		return nil, fmt.Errorf("gocam: insufficient buffers: %d", req.Count)
	}

	buffers = make([]mappedBuffer, req.Count)

	for i := uint32(0); i < req.Count; i++ {
		buf := v4l2Buffer{
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMMap,
			Index:  i,
		}
		if err := ioctl(fd, vidiocQuerybuf, unsafe.Pointer(&buf)); err != nil {
			cleanup()
			return nil, fmt.Errorf("gocam: VIDIOC_QUERYBUF index %d failed: %w", i, err)
		}

		data, err := syscall.Mmap(fd, int64(buf.Offset), int(buf.Length), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("gocam: mmap buffer %d failed: %w", i, err)
		}

		buffers[i] = mappedBuffer{data: data, length: buf.Length}

		if err := ioctl(fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
			cleanup()
			return nil, fmt.Errorf("gocam: VIDIOC_QBUF index %d failed: %w", i, err)
		}
	}

	bufType := uint32(v4l2BufTypeVideoCapture)
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&bufType)); err != nil {
		cleanup()
		return nil, fmt.Errorf("gocam: VIDIOC_STREAMON failed: %w", err)
	}
	streamStarted = true

	frameW := int(width)
	frameH := int(height)
	if frameW <= 0 || frameH <= 0 {
		cleanup()
		return nil, fmt.Errorf("gocam: invalid frame size %dx%d", frameW, frameH)
	}

	// Logical output size:
	// - If the source is larger than CIF in at least one dimension, we downsample to CIF.
	// - Otherwise, keep the native size (no upscaling).
	outW := frameW
	outH := frameH
	if frameW > cifWidth || frameH > cifHeight {
		outW = cifWidth
		outH = cifHeight
	}

	logCameraConfig(&caps, pixelFormat, outW, outH, stride)

	frames := make(chan Frame, 1)

	go func() {
		defer close(frames)
		defer cleanup()

		const dropThreshold = 30
		misses := 0

		sendFrame := func(frame Frame) {
			select {
			case frames <- frame:
			default:
				<-frames
				frames <- frame
			}
		}

		makeBlack := func() []byte {
			size := outW * outH * 3
			if size <= 0 {
				return nil
			}
			buf := make([]byte, size)
			for i := 0; i < size; i += 3 {
				buf[i] = 16
				buf[i+1] = 128
				buf[i+2] = 128
			}
			return buf
		}

		sendBlack := func() bool {
			if outW <= 0 || outH <= 0 {
				return false
			}
			data := makeBlack()
			if data == nil {
				return false
			}
			frame := Frame{
				Data:   data,
				Width:  outW,
				Height: outH,
			}
			sendFrame(frame)
			return true
		}

		handleDrop := func(longWait, shortWait time.Duration) {
			misses++
			if misses >= dropThreshold && sendBlack() {
				time.Sleep(longWait)
			} else {
				time.Sleep(shortWait)
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			buf := v4l2Buffer{
				Type:   v4l2BufTypeVideoCapture,
				Memory: v4l2MemoryMMap,
			}

			if err := ioctl(fd, vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
				if errno, ok := err.(syscall.Errno); ok && (errno == syscall.EAGAIN || errno == syscall.EINTR) {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				handleDrop(33*time.Millisecond, 10*time.Millisecond)
				continue
			}

			index := buf.Index
			if int(index) >= len(buffers) {
				_ = ioctl(fd, vidiocQBuf, unsafe.Pointer(&buf))
				continue
			}

			data := buffers[index].data
			sz := int(buf.Bytesused)
			if sz <= 0 || sz > len(data) {
				sz = len(data)
			}
			src := data[:sz]

			frameData := convertFrame(src, pixelFormat, frameW, frameH, stride)

			if err := ioctl(fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
				return
			}

			if frameData == nil {
				handleDrop(33*time.Millisecond, 5*time.Millisecond)
				continue
			}

			// Downsample with aspect-ratio-preserving center crop if needed.
			dataOut := frameData
			w := frameW
			h := frameH
			if frameW > cifWidth || frameH > cifHeight {
				resampled := resampleYCbCr444Fill(frameData, frameW, frameH, cifWidth, cifHeight)
				if resampled == nil {
					handleDrop(33*time.Millisecond, 5*time.Millisecond)
					continue
				}
				dataOut = resampled
				w = cifWidth
				h = cifHeight
			}

			frame := Frame{
				Data:   dataOut,
				Width:  w,
				Height: h,
			}

			misses = 0
			sendFrame(frame)
		}
	}()

	return frames, nil
}

// convertFrame normalizes a single captured frame from various V4L2 pixel
// formats into a tightly packed YCbCr 4:4:4 buffer (packed Y, Cb, Cr per pixel).
func convertFrame(src []byte, pixFmt uint32, width, height, stride int) []byte {
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
	case v4l2PixFmtYUV24:
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

	case v4l2PixFmtNV12:
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

	case v4l2PixFmtYUYV:
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

	case v4l2PixFmtRGB24:
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
func resampleYCbCr444Fill(src []byte, srcW, srcH, dstW, dstH int) []byte {
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

func clampToByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
