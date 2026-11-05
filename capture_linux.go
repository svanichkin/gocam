//go:build linux
// +build linux

package gocam

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// V4L2 backend.
//
// Finding a camera: GOCAM_DEVICE (e.g. /dev/video2) if set, otherwise every
// /dev/video* node in numeric order. Nodes that are not capture devices
// (video decoders and encoders such as the PinePhone's Cedrus at
// /dev/video0, metadata nodes) are skipped.
//
// Opening one: the formats the driver lists (VIDIOC_ENUM_FMT) are tried in
// gocam's order of preference, first at CIF (352x288), then at the node's
// current size; then the node's current format as it is (v4l2loopback
// devices, whose format their producer sets). On SoC cameras (PinePhone, many ARM boards) the node is fed
// by a media controller pipeline: if streaming does not start, gocam links a
// sensor in (GOCAM_SENSOR picks one by name; the front camera is preferred),
// matches the formats along the pipeline and tries again at the sensor's
// size. A working camera is opened exactly as before; the extra steps only
// run when the plain way fails.

const (
	v4l2BufTypeVideoCapture       = 1
	v4l2BufTypeVideoCaptureMPlane = 9
	v4l2FieldAny                  = 0
	v4l2MemoryMMap                = 1
)

const (
	v4l2CapVideoCapture       = 0x00000001
	v4l2CapVideoCaptureMPlane = 0x00001000
	v4l2CapVideoM2MMPlane     = 0x00004000
	v4l2CapVideoM2M           = 0x00008000
	v4l2CapStreaming          = 0x04000000
	v4l2CapDeviceCaps         = 0x80000000
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

type v4l2PlanePixFormat struct {
	Sizeimage    uint32
	Bytesperline uint32
	Reserved     [6]uint16
}

type v4l2PixFormatMPlane struct {
	Width        uint32
	Height       uint32
	Pixelformat  uint32
	Field        uint32
	Colorspace   uint32
	PlaneFmt     [8]v4l2PlanePixFormat
	NumPlanes    uint8
	Flags        uint8
	YcbcrEnc     uint8
	Quantization uint8
	XferFunc     uint8
	Reserved     [7]uint8
}

type v4l2Format struct {
	Type uint32
	_    [4]byte // align union to 64-bit boundary like C's struct v4l2_format
	fmt  [200]byte
}

type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	Pixelformat uint32
	MbusCode    uint32
	Reserved    [3]uint32
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
	M         uintptr // union: offset (single-planar) or planes pointer
	Length    uint32
	Reserved2 uint32
	Reserved  uint32
}

// v4l2Plane is struct v4l2_plane (64 bytes); the m union sits at offset 8.
type v4l2Plane struct {
	Bytesused  uint32
	Length     uint32
	MemOffset  uint32
	_          uint32
	DataOffset uint32
	Reserved   [11]uint32
}

const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

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

func iow(typ, nr, size uintptr) uintptr  { return ioc(iocWrite, typ, nr, size) }
func ior(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }
func iowr(typ, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, typ, nr, size) }

var (
	vidiocQuerycap  = ior(uintptr('V'), 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt   = iowr(uintptr('V'), 2, unsafe.Sizeof(v4l2FmtDesc{}))
	vidiocGFmt      = iowr(uintptr('V'), 4, unsafe.Sizeof(v4l2Format{}))
	vidiocSFmt      = iowr(uintptr('V'), 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs   = iowr(uintptr('V'), 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQuerybuf  = iowr(uintptr('V'), 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf      = iowr(uintptr('V'), 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf     = iowr(uintptr('V'), 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn  = iow(uintptr('V'), 18, unsafe.Sizeof(uint32(0)))
	vidiocStreamOff = iow(uintptr('V'), 19, unsafe.Sizeof(uint32(0)))
)

var camLog = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

func v4l2CString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// v4l2Stream is an open, streaming capture node.
type v4l2Stream struct {
	path    string
	fd      int
	caps    v4l2Capability
	bufType uint32
	pixFmt  uint32
	width   int
	height  int
	stride  int
	buffers [][]byte
	planes  *v4l2Plane // scratch plane for multi-planar dequeues
}

// StartStream finds a camera, configures a V4L2 capture stream, and returns
// a channel of frames encoded as tightly packed YCbCr 4:4:4 buffers.
func StartStream(ctx context.Context) (<-chan Frame, error) {
	paths := candidateDevices()
	if len(paths) == 0 {
		return nil, errors.New("gocam: no /dev/video* devices (is a camera driver loaded?)")
	}
	var failures []string
	for _, path := range paths {
		s, err := openStream(path)
		if err != nil {
			camLog.Printf("[gocam] %s: %v", path, err)
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		s.logConfig()
		return s.run(ctx), nil
	}
	return nil, fmt.Errorf("gocam: no usable camera (%s)", strings.Join(failures, "; "))
}

// candidateDevices lists GOCAM_DEVICE alone if set, otherwise every
// /dev/videoN in numeric order.
func candidateDevices() []string {
	if dev := strings.TrimSpace(os.Getenv("GOCAM_DEVICE")); dev != "" {
		return []string{dev}
	}
	paths, _ := filepath.Glob("/dev/video*")
	num := func(p string) int {
		n, err := strconv.Atoi(strings.TrimPrefix(p, "/dev/video"))
		if err != nil {
			return 1 << 30
		}
		return n
	}
	sort.Slice(paths, func(i, j int) bool { return num(paths[i]) < num(paths[j]) })
	return paths
}

func openStream(path string) (*v4l2Stream, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EACCES) {
			return nil, fmt.Errorf("cannot open: %w (is the user in the 'video' group?)", err)
		}
		return nil, fmt.Errorf("cannot open: %w", err)
	}
	s := &v4l2Stream{path: path, fd: fd}

	if err := ioctl(fd, vidiocQuerycap, unsafe.Pointer(&s.caps)); err != nil {
		s.close()
		return nil, fmt.Errorf("VIDIOC_QUERYCAP: %w", err)
	}
	caps := s.caps.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = s.caps.DeviceCaps
	}
	card := v4l2CString(s.caps.Card[:])
	switch {
	case caps&(v4l2CapVideoM2M|v4l2CapVideoM2MMPlane) != 0:
		s.close()
		return nil, fmt.Errorf("%q is a memory-to-memory device (codec), not a camera", card)
	case caps&v4l2CapVideoCapture != 0:
		s.bufType = v4l2BufTypeVideoCapture
	case caps&v4l2CapVideoCaptureMPlane != 0:
		s.bufType = v4l2BufTypeVideoCaptureMPlane
	default:
		s.close()
		return nil, fmt.Errorf("%q does not capture video", card)
	}
	if caps&v4l2CapStreaming == 0 {
		s.close()
		return nil, fmt.Errorf("%q does not support streaming I/O", card)
	}

	formats := s.listFormats()
	curW, curH := s.currentSize()

	// Plain attempts: CIF, then the node's own size.
	sizes := [][2]int{{cifWidth, cifHeight}}
	if curW > 0 && curH > 0 && (curW != cifWidth || curH != cifHeight) {
		sizes = append(sizes, [2]int{curW, curH})
	}
	var lastErr error
	for _, sz := range sizes {
		if lastErr = s.tryStart(formats, sz[0], sz[1]); lastErr == nil {
			return s, nil
		}
	}

	// The node as it is: a v4l2loopback device fed by another program
	// refuses VIDIOC_S_FMT; its format is the one the producer chose.
	if err := s.startAsIs(); err == nil {
		return s, nil
	} else {
		camLog.Printf("[gocam] %s: current format: %v", path, err)
	}

	// A media controller pipeline (SoC camera): link a sensor in, match the
	// formats and try at the sensor's size.
	if w, h, sensorFormats, err := setupMediaPipeline(fd); err != nil {
		camLog.Printf("[gocam] %s: media pipeline: %v", path, err)
	} else {
		try := sensorFormats
		for _, f := range formats {
			if !containsFmt(try, f) {
				try = append(try, f)
			}
		}
		if lastErr = s.tryStart(try, w, h); lastErr == nil {
			return s, nil
		}
	}
	s.close()
	return nil, lastErr
}

// listFormats returns the driver's formats that gocam can convert, in
// gocam's order of preference; all of them if the driver lists none.
func (s *v4l2Stream) listFormats() []uint32 {
	var listed []uint32
	for i := uint32(0); i < 64; i++ {
		d := v4l2FmtDesc{Index: i, Type: s.bufType}
		if ioctl(s.fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			break
		}
		listed = append(listed, d.Pixelformat)
	}
	if len(listed) == 0 {
		return append([]uint32(nil), preferredPixFormats...)
	}
	var out []uint32
	for _, f := range preferredPixFormats {
		if containsFmt(listed, f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		names := make([]string, len(listed))
		for i, f := range listed {
			names[i] = pixFmtName(f)
		}
		camLog.Printf("[gocam] %s offers only %s, which gocam cannot convert", s.path, strings.Join(names, ", "))
	}
	return out
}

func containsFmt(list []uint32, f uint32) bool {
	for _, x := range list {
		if x == f {
			return true
		}
	}
	return false
}

// currentSize is the node's configured size (0 if unknown).
func (s *v4l2Stream) currentSize() (int, int) {
	f := v4l2Format{Type: s.bufType}
	if ioctl(s.fd, vidiocGFmt, unsafe.Pointer(&f)) != nil {
		return 0, 0
	}
	if s.bufType == v4l2BufTypeVideoCaptureMPlane {
		mp := (*v4l2PixFormatMPlane)(unsafe.Pointer(&f.fmt[0]))
		return int(mp.Width), int(mp.Height)
	}
	pix := (*v4l2PixFormat)(unsafe.Pointer(&f.fmt[0]))
	return int(pix.Width), int(pix.Height)
}

// startAsIs streams in the node's current format, without setting one.
func (s *v4l2Stream) startAsIs() error {
	f := v4l2Format{Type: s.bufType}
	if err := ioctl(s.fd, vidiocGFmt, unsafe.Pointer(&f)); err != nil {
		return fmt.Errorf("VIDIOC_G_FMT: %w", err)
	}
	if s.bufType == v4l2BufTypeVideoCaptureMPlane {
		mp := (*v4l2PixFormatMPlane)(unsafe.Pointer(&f.fmt[0]))
		if mp.NumPlanes != 1 {
			return fmt.Errorf("%s uses %d planes; gocam reads one", pixFmtName(mp.Pixelformat), mp.NumPlanes)
		}
		s.pixFmt, s.width, s.height, s.stride = mp.Pixelformat, int(mp.Width), int(mp.Height), int(mp.PlaneFmt[0].Bytesperline)
	} else {
		pix := (*v4l2PixFormat)(unsafe.Pointer(&f.fmt[0]))
		s.pixFmt, s.width, s.height, s.stride = pix.Pixelformat, int(pix.Width), int(pix.Height), int(pix.Bytesperline)
	}
	if !containsFmt(preferredPixFormats, s.pixFmt) {
		return fmt.Errorf("%s %dx%d, which gocam cannot convert", pixFmtName(s.pixFmt), s.width, s.height)
	}
	if s.width <= 0 || s.height <= 0 {
		return fmt.Errorf("invalid frame size %dx%d", s.width, s.height)
	}
	if s.stride < bytesPerLine(s.pixFmt, s.width) {
		s.stride = bytesPerLine(s.pixFmt, s.width)
	}
	if err := s.startStreaming(); err != nil {
		s.stopStreaming()
		return fmt.Errorf("%s %dx%d: %w", pixFmtName(s.pixFmt), s.width, s.height, err)
	}
	return nil
}

// tryStart sets the first format the driver accepts at about w x h and
// starts streaming; it tries the next format if streaming is refused.
func (s *v4l2Stream) tryStart(formats []uint32, w, h int) error {
	if len(formats) == 0 {
		return errors.New("no convertible pixel format")
	}
	var lastErr error
	for _, pf := range formats {
		if err := s.setFormat(pf, w, h); err != nil {
			lastErr = err
			continue
		}
		if err := s.startStreaming(); err != nil {
			lastErr = fmt.Errorf("%s %dx%d: %w", pixFmtName(s.pixFmt), s.width, s.height, err)
			s.stopStreaming()
			continue
		}
		return nil
	}
	return lastErr
}

// setFormat asks for pf at w x h; the driver may adjust the size, but a
// different pixel format is refused.
func (s *v4l2Stream) setFormat(pf uint32, w, h int) error {
	f := v4l2Format{Type: s.bufType}
	if s.bufType == v4l2BufTypeVideoCaptureMPlane {
		mp := (*v4l2PixFormatMPlane)(unsafe.Pointer(&f.fmt[0]))
		mp.Width, mp.Height, mp.Pixelformat, mp.Field, mp.NumPlanes = uint32(w), uint32(h), pf, v4l2FieldAny, 1
		if err := ioctl(s.fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
			return fmt.Errorf("VIDIOC_S_FMT %s: %w", pixFmtName(pf), err)
		}
		if mp.Pixelformat != pf {
			return fmt.Errorf("driver gave %s instead of %s", pixFmtName(mp.Pixelformat), pixFmtName(pf))
		}
		if mp.NumPlanes != 1 {
			return fmt.Errorf("%s uses %d planes; gocam reads one", pixFmtName(pf), mp.NumPlanes)
		}
		s.pixFmt, s.width, s.height, s.stride = pf, int(mp.Width), int(mp.Height), int(mp.PlaneFmt[0].Bytesperline)
	} else {
		pix := (*v4l2PixFormat)(unsafe.Pointer(&f.fmt[0]))
		pix.Width, pix.Height, pix.Pixelformat, pix.Field = uint32(w), uint32(h), pf, v4l2FieldAny
		if err := ioctl(s.fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
			return fmt.Errorf("VIDIOC_S_FMT %s: %w", pixFmtName(pf), err)
		}
		if pix.Pixelformat != pf {
			return fmt.Errorf("driver gave %s instead of %s", pixFmtName(pix.Pixelformat), pixFmtName(pf))
		}
		s.pixFmt, s.width, s.height, s.stride = pf, int(pix.Width), int(pix.Height), int(pix.Bytesperline)
	}
	if s.width <= 0 || s.height <= 0 {
		return fmt.Errorf("invalid frame size %dx%d", s.width, s.height)
	}
	if s.stride < bytesPerLine(s.pixFmt, s.width) {
		s.stride = bytesPerLine(s.pixFmt, s.width)
	}
	return nil
}

func (s *v4l2Stream) newBuffer(index uint32) *v4l2Buffer {
	b := &v4l2Buffer{Type: s.bufType, Memory: v4l2MemoryMMap, Index: index}
	if s.bufType == v4l2BufTypeVideoCaptureMPlane {
		if s.planes == nil {
			s.planes = new(v4l2Plane)
		}
		*s.planes = v4l2Plane{}
		b.M = uintptr(unsafe.Pointer(s.planes))
		b.Length = 1
	}
	return b
}

// startStreaming maps four buffers, queues them and starts the stream.
func (s *v4l2Stream) startStreaming() error {
	req := v4l2RequestBuffers{Count: 4, Type: s.bufType, Memory: v4l2MemoryMMap}
	if err := ioctl(s.fd, vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("VIDIOC_REQBUFS: %w", err)
	}
	if req.Count < 2 {
		return fmt.Errorf("insufficient buffers: %d", req.Count)
	}
	for i := uint32(0); i < req.Count; i++ {
		b := s.newBuffer(i)
		err := ioctl(s.fd, vidiocQuerybuf, unsafe.Pointer(b))
		runtime.KeepAlive(s.planes)
		if err != nil {
			return fmt.Errorf("VIDIOC_QUERYBUF %d: %w", i, err)
		}
		offset, length := uint32(b.M), b.Length
		if s.bufType == v4l2BufTypeVideoCaptureMPlane {
			offset, length = s.planes.MemOffset, s.planes.Length
		}
		data, err := syscall.Mmap(s.fd, int64(offset), int(length), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		s.buffers = append(s.buffers, data)
		b = s.newBuffer(i)
		err = ioctl(s.fd, vidiocQBuf, unsafe.Pointer(b))
		runtime.KeepAlive(s.planes)
		if err != nil {
			return fmt.Errorf("VIDIOC_QBUF %d: %w", i, err)
		}
	}
	bufType := s.bufType
	if err := ioctl(s.fd, vidiocStreamOn, unsafe.Pointer(&bufType)); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENOLINK) {
			return fmt.Errorf("VIDIOC_STREAMON: %w (the camera pipeline is not linked or its formats do not match)", err)
		}
		return fmt.Errorf("VIDIOC_STREAMON: %w", err)
	}
	return nil
}

// stopStreaming stops the stream and releases the buffers, so the format can
// be changed and streaming tried again.
func (s *v4l2Stream) stopStreaming() {
	bufType := s.bufType
	_ = ioctl(s.fd, vidiocStreamOff, unsafe.Pointer(&bufType))
	for _, b := range s.buffers {
		_ = syscall.Munmap(b)
	}
	s.buffers = nil
	req := v4l2RequestBuffers{Count: 0, Type: s.bufType, Memory: v4l2MemoryMMap}
	_ = ioctl(s.fd, vidiocReqbufs, unsafe.Pointer(&req))
}

func (s *v4l2Stream) close() {
	if s.buffers != nil {
		s.stopStreaming()
	}
	_ = syscall.Close(s.fd)
}

func (s *v4l2Stream) logConfig() {
	outW, outH := outputSize(s.width, s.height)
	planar := ""
	if s.bufType == v4l2BufTypeVideoCaptureMPlane {
		planar = " (multi-planar API)"
	}
	camLog.Println("[gocam] [V4L2]")
	camLog.Printf("[gocam]   %s (Capture)%s\n", s.path, planar)
	camLog.Printf("[gocam]     Card:        %s\n", v4l2CString(s.caps.Card[:]))
	camLog.Printf("[gocam]     Driver:      %s\n", v4l2CString(s.caps.Driver[:]))
	camLog.Printf("[gocam]     Bus:         %s\n", v4l2CString(s.caps.BusInfo[:]))
	camLog.Printf("[gocam]     Format:      %s %dx%d (stride %d) -> YCbCr 4:4:4 %dx%d\n",
		pixFmtName(s.pixFmt), s.width, s.height, s.stride, outW, outH)
}

// run reads frames until ctx is done; it owns and finally closes s.
func (s *v4l2Stream) run(ctx context.Context) <-chan Frame {
	frames := make(chan Frame, 1)
	go func() {
		defer close(frames)
		defer s.close()

		sendFrame := func(frame Frame) {
			select {
			case frames <- frame:
			default:
				<-frames
				frames <- frame
			}
		}

		outW, outH := outputSize(s.width, s.height)
		black := func() Frame {
			buf := make([]byte, outW*outH*3)
			for i := 0; i < len(buf); i += 3 {
				buf[i], buf[i+1], buf[i+2] = 16, 128, 128
			}
			return Frame{Data: buf, Width: outW, Height: outH}
		}

		// Wait for frames with ppoll (100 ms steps, to notice ctx); after
		// about 300 ms without one, black frames stand in for the camera.
		const (
			waitStep      = 100 * time.Millisecond
			dropThreshold = 3
		)
		misses := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			ready, err := waitReadable(s.fd, waitStep)
			if err != nil {
				camLog.Printf("[gocam] %s: poll: %v", s.path, err)
				return
			}
			if !ready {
				misses++
				if misses >= dropThreshold {
					sendFrame(black())
				}
				continue
			}

			b := s.newBuffer(0)
			err = ioctl(s.fd, vidiocDQBuf, unsafe.Pointer(b))
			runtime.KeepAlive(s.planes)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
					continue
				}
				camLog.Printf("[gocam] %s: VIDIOC_DQBUF: %v", s.path, err)
				misses++
				time.Sleep(10 * time.Millisecond)
				continue
			}

			index := b.Index
			used, offset := int(b.Bytesused), 0
			if s.bufType == v4l2BufTypeVideoCaptureMPlane {
				used, offset = int(s.planes.Bytesused), int(s.planes.DataOffset)
			}
			var data []byte
			if int(index) < len(s.buffers) {
				buf := s.buffers[index]
				if used <= 0 || used > len(buf) {
					used = len(buf)
				}
				if offset < used {
					data, outW, outH = sampleYCbCr444(buf[offset:used], s.pixFmt, s.width, s.height, s.stride)
				}
			}

			b = s.newBuffer(index)
			err = ioctl(s.fd, vidiocQBuf, unsafe.Pointer(b))
			runtime.KeepAlive(s.planes)
			if err != nil {
				camLog.Printf("[gocam] %s: VIDIOC_QBUF: %v", s.path, err)
				return
			}

			if data == nil {
				misses++
				continue
			}
			misses = 0
			sendFrame(Frame{Data: data, Width: outW, Height: outH})
		}
	}()
	return frames
}

// waitReadable waits until fd has a frame or timeout passes.
func waitReadable(fd int, timeout time.Duration) (bool, error) {
	pfd := struct {
		fd      int32
		events  int16
		revents int16
	}{fd: int32(fd), events: 0x1 /* POLLIN */}
	ts := syscall.NsecToTimespec(int64(timeout))
	n, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&pfd)), 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	if errno != 0 {
		if errno == syscall.EINTR {
			return false, nil
		}
		return false, errno
	}
	if pfd.revents&(0x8|0x20) != 0 { // POLLERR, POLLNVAL
		return false, fmt.Errorf("device error (revents 0x%x)", pfd.revents)
	}
	return n > 0, nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
