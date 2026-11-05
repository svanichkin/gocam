//go:build linux
// +build linux

package gocam

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// Media controller support for SoC cameras (PinePhone and similar): the
// capture node is fed by a sensor through a pipeline of sub-devices. The
// sensor's link must be enabled and every pad along the way must carry the
// same format, or VIDIOC_STREAMON fails with EPIPE. setupMediaPipeline does
// what `media-ctl --links ... --set-v4l2 ...` would. It only runs when the
// node does not stream as it is.

type mediaEntityDesc struct {
	ID       uint32
	Name     [32]byte
	Type     uint32
	Revision uint32
	Flags    uint32
	GroupID  uint32
	Pads     uint16
	Links    uint16
	Reserved [4]uint32
	Raw      [184]byte // union; dev.major, dev.minor are the first two u32
}

type mediaPadDesc struct {
	Entity   uint32
	Index    uint16
	_        uint16
	Flags    uint32
	Reserved [2]uint32
}

type mediaLinkDesc struct {
	Source   mediaPadDesc
	Sink     mediaPadDesc
	Flags    uint32
	Reserved [2]uint32
}

type mediaLinksEnum struct {
	Entity   uint32
	Pads     uintptr // *mediaPadDesc
	Links    uintptr // *mediaLinkDesc
	Reserved [4]uint32
}

type v4l2MbusFramefmt struct {
	Width        uint32
	Height       uint32
	Code         uint32
	Field        uint32
	Colorspace   uint32
	YcbcrEnc     uint16
	Quantization uint16
	XferFunc     uint16
	Flags        uint16
	Reserved     [10]uint16
}

type v4l2SubdevFormat struct {
	Which    uint32
	Pad      uint32
	Format   v4l2MbusFramefmt
	Stream   uint32
	Reserved [7]uint32
}

const (
	mediaEntIDFlagNext     = 1 << 31
	v4l2SubdevFormatActive = 1
	mediaPadFlSource       = 2
	sysfsCharDevicesDir    = "/sys/dev/char"
	mediaIOCEnumEntitiesNr = 1
	mediaIOCEnumLinksNr    = 2
	mediaIOCSetupLinkNr    = 3
	vidiocSubdevGFmtNr     = 4
	vidiocSubdevSFmtNr     = 5
	mediaIOCType           = '|'
	videoIOCType           = 'V'
)

var (
	mediaIOCEnumEntities = iowr(mediaIOCType, mediaIOCEnumEntitiesNr, unsafe.Sizeof(mediaEntityDesc{}))
	mediaIOCEnumLinks    = iowr(mediaIOCType, mediaIOCEnumLinksNr, unsafe.Sizeof(mediaLinksEnum{}))
	mediaIOCSetupLink    = iowr(mediaIOCType, mediaIOCSetupLinkNr, unsafe.Sizeof(mediaLinkDesc{}))
	vidiocSubdevGFmt     = iowr(videoIOCType, vidiocSubdevGFmtNr, unsafe.Sizeof(v4l2SubdevFormat{}))
	vidiocSubdevSFmt     = iowr(videoIOCType, vidiocSubdevSFmtNr, unsafe.Sizeof(v4l2SubdevFormat{}))
)

// setupMediaPipeline links a sensor to the capture node fd and matches the
// formats along the way. It returns the size and the pixel formats to try
// on the capture node.
func setupMediaPipeline(videoFd int) (int, int, []uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(videoFd, &st); err != nil {
		return 0, 0, nil, err
	}
	vMajor, vMinor := devMajor(uint64(st.Rdev)), devMinor(uint64(st.Rdev))

	medias, _ := filepath.Glob("/dev/media*")
	if len(medias) == 0 {
		return 0, 0, nil, errors.New("no /dev/media* device (not a media controller camera)")
	}
	for _, mpath := range medias {
		mfd, err := syscall.Open(mpath, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			camLog.Printf("[gocam] %s: %v", mpath, err)
			continue
		}
		entities, links, err := mediaTopology(mfd)
		if err != nil {
			syscall.Close(mfd)
			camLog.Printf("[gocam] %s: %v", mpath, err)
			continue
		}
		videoID, found := uint32(0), false
		for _, e := range entities {
			if e.major == vMajor && e.minor == vMinor {
				videoID, found = e.id, true
				break
			}
		}
		if !found {
			syscall.Close(mfd)
			continue
		}
		logMediaTopology(mpath, entities, links)
		w, h, formats, err := routeAndFormat(mfd, entities, links, videoID)
		syscall.Close(mfd)
		return w, h, formats, err
	}
	return 0, 0, nil, fmt.Errorf("no media device contains the capture node %d:%d", vMajor, vMinor)
}

func mediaTopology(mfd int) ([]mediaEntity, []mediaLink, error) {
	var entities []mediaEntity
	var descs []mediaEntityDesc
	id := uint32(0)
	for i := 0; i < 256; i++ {
		d := mediaEntityDesc{ID: id | mediaEntIDFlagNext}
		if err := ioctl(mfd, mediaIOCEnumEntities, unsafe.Pointer(&d)); err != nil {
			if errors.Is(err, syscall.EINVAL) {
				break // no more entities
			}
			return nil, nil, fmt.Errorf("MEDIA_IOC_ENUM_ENTITIES: %w", err)
		}
		id = d.ID
		major := *(*uint32)(unsafe.Pointer(&d.Raw[0]))
		minor := *(*uint32)(unsafe.Pointer(&d.Raw[4]))
		entities = append(entities, mediaEntity{id: d.ID, name: v4l2CString(d.Name[:]), function: d.Type, major: major, minor: minor})
		descs = append(descs, d)
	}
	if len(entities) == 0 {
		return nil, nil, errors.New("no media entities")
	}
	var links []mediaLink
	for _, d := range descs {
		if d.Links == 0 {
			continue
		}
		pads := make([]mediaPadDesc, max(int(d.Pads), 1))
		ls := make([]mediaLinkDesc, d.Links)
		e := mediaLinksEnum{Entity: d.ID, Pads: uintptr(unsafe.Pointer(&pads[0])), Links: uintptr(unsafe.Pointer(&ls[0]))}
		err := ioctl(mfd, mediaIOCEnumLinks, unsafe.Pointer(&e))
		runtime.KeepAlive(pads)
		runtime.KeepAlive(ls)
		if err != nil {
			return nil, nil, fmt.Errorf("MEDIA_IOC_ENUM_LINKS %q: %w", v4l2CString(d.Name[:]), err)
		}
		for _, l := range ls {
			// Each entity lists its outgoing and incoming links; keep the
			// outgoing ones so every link is seen once.
			if l.Source.Entity != d.ID {
				continue
			}
			links = append(links, mediaLink{srcEntity: l.Source.Entity, srcPad: l.Source.Index, sinkEnt: l.Sink.Entity, sinkPad: l.Sink.Index, flags: l.Flags})
		}
	}
	return entities, links, nil
}

func logMediaTopology(mpath string, entities []mediaEntity, links []mediaLink) {
	names := map[uint32]string{}
	for _, e := range entities {
		names[e.id] = e.name
		kind := ""
		if e.function == mediaEntFCamSensor {
			kind = " [sensor]"
		}
		camLog.Printf("[gocam] %s: entity %d %q%s (dev %d:%d)", mpath, e.id, e.name, kind, e.major, e.minor)
	}
	for _, l := range links {
		state := "disabled"
		if l.enabled() {
			state = "enabled"
		}
		if l.immutable() {
			state += ", immutable"
		}
		camLog.Printf("[gocam] %s: link %q:%d -> %q:%d (%s)", mpath, names[l.srcEntity], l.srcPad, names[l.sinkEnt], l.sinkPad, state)
	}
}

func routeAndFormat(mfd int, entities []mediaEntity, links []mediaLink, videoID uint32) (int, int, []uint32, error) {
	plan, ok := planMediaRoute(entities, links, videoID, strings.TrimSpace(os.Getenv("GOCAM_SENSOR")))
	if !ok {
		return 0, 0, nil, errors.New("no camera sensor is connected to this capture node")
	}
	camLog.Printf("[gocam] using sensor %q (set GOCAM_SENSOR to choose another)", plan.sensor.name)
	for _, l := range plan.disable {
		if err := setupLink(mfd, l, false); err != nil {
			camLog.Printf("[gocam] unlink %d:%d -> %d:%d: %v", l.srcEntity, l.srcPad, l.sinkEnt, l.sinkPad, err)
		}
	}
	for _, l := range plan.enable {
		if err := setupLink(mfd, l, true); err != nil {
			return 0, 0, nil, fmt.Errorf("enable link %d:%d -> %d:%d: %w", l.srcEntity, l.srcPad, l.sinkEnt, l.sinkPad, err)
		}
	}

	byID := map[uint32]mediaEntity{}
	for _, e := range entities {
		byID[e.id] = e
	}

	// The sensor's output format: YUV or RGB as is; anything else (raw
	// Bayer) is switched to YUV 4:2:2 if the sensor can do it.
	sensorFd, err := openSubdev(plan.sensor)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("sensor %q: %w", plan.sensor.name, err)
	}
	defer syscall.Close(sensorFd)
	srcPad := uint32(plan.path[0].srcPad)
	mf, err := subdevFormat(sensorFd, srcPad)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("sensor %q format: %w", plan.sensor.name, err)
	}
	if !isYUVOrRGBMbus(mf.Code) {
		for _, code := range []uint32{mbusUYVY8_2X8, mbusYUYV8_2X8} {
			want := mf
			want.Code = code
			if got, err := setSubdevFormat(sensorFd, srcPad, want); err == nil && isYUVOrRGBMbus(got.Code) {
				mf = got
				break
			}
		}
		if !isYUVOrRGBMbus(mf.Code) {
			return 0, 0, nil, fmt.Errorf("sensor %q outputs bus format 0x%x (raw Bayer?), which needs an ISP", plan.sensor.name, mf.Code)
		}
	}
	camLog.Printf("[gocam] sensor %q: %dx%d, bus format 0x%x", plan.sensor.name, mf.Width, mf.Height, mf.Code)

	// Every sub-device between the sensor and the capture node gets the
	// same format on its input and output pads.
	for i, l := range plan.path {
		if l.sinkEnt == videoID {
			break
		}
		ent := byID[l.sinkEnt]
		fd, err := openSubdev(ent)
		if err != nil {
			camLog.Printf("[gocam] %q: %v", ent.name, err)
			continue
		}
		if got, err := setSubdevFormat(fd, uint32(l.sinkPad), mf); err != nil {
			camLog.Printf("[gocam] %q pad %d: %v", ent.name, l.sinkPad, err)
		} else {
			mf = got
		}
		if i+1 < len(plan.path) {
			out := uint32(plan.path[i+1].srcPad)
			if got, err := setSubdevFormat(fd, out, mf); err == nil {
				mf = got
			} else if got, err := subdevFormat(fd, out); err == nil {
				mf = got // source pads often follow their sink and are read-only
			}
		}
		syscall.Close(fd)
	}
	return int(mf.Width), int(mf.Height), pixFormatsForMbus(mf.Code), nil
}

func setupLink(mfd int, l mediaLink, enable bool) error {
	d := mediaLinkDesc{
		Source: mediaPadDesc{Entity: l.srcEntity, Index: l.srcPad, Flags: mediaPadFlSource},
		Sink:   mediaPadDesc{Entity: l.sinkEnt, Index: l.sinkPad, Flags: 1},
	}
	if enable {
		d.Flags = mediaLnkFlEnabled
	}
	return ioctl(mfd, mediaIOCSetupLink, unsafe.Pointer(&d))
}

// openSubdev opens the /dev/v4l-subdevN node of a media entity, found
// through sysfs by its device number.
func openSubdev(e mediaEntity) (int, error) {
	if e.major == 0 && e.minor == 0 {
		return -1, errors.New("entity has no device node")
	}
	uevent, err := os.ReadFile(fmt.Sprintf("%s/%d:%d/uevent", sysfsCharDevicesDir, e.major, e.minor))
	if err != nil {
		return -1, err
	}
	for _, line := range strings.Split(string(uevent), "\n") {
		if name, ok := strings.CutPrefix(line, "DEVNAME="); ok {
			fd, err := syscall.Open("/dev/"+strings.TrimSpace(name), syscall.O_RDWR|syscall.O_CLOEXEC, 0)
			if err != nil {
				return -1, fmt.Errorf("open /dev/%s: %w", strings.TrimSpace(name), err)
			}
			return fd, nil
		}
	}
	return -1, errors.New("no DEVNAME in sysfs")
}

func subdevFormat(fd int, pad uint32) (v4l2MbusFramefmt, error) {
	f := v4l2SubdevFormat{Which: v4l2SubdevFormatActive, Pad: pad}
	if err := ioctl(fd, vidiocSubdevGFmt, unsafe.Pointer(&f)); err != nil {
		return v4l2MbusFramefmt{}, fmt.Errorf("VIDIOC_SUBDEV_G_FMT pad %d: %w", pad, err)
	}
	return f.Format, nil
}

// setSubdevFormat sets a pad's format and returns what the driver chose.
func setSubdevFormat(fd int, pad uint32, mf v4l2MbusFramefmt) (v4l2MbusFramefmt, error) {
	f := v4l2SubdevFormat{Which: v4l2SubdevFormatActive, Pad: pad, Format: mf}
	if err := ioctl(fd, vidiocSubdevSFmt, unsafe.Pointer(&f)); err != nil {
		return v4l2MbusFramefmt{}, fmt.Errorf("VIDIOC_SUBDEV_S_FMT pad %d: %w", pad, err)
	}
	return f.Format, nil
}

// devMajor and devMinor split a Linux device number (glibc encoding).
func devMajor(dev uint64) uint32 {
	return uint32((dev>>8)&0xfff) | uint32((dev>>32)&^0xfff)
}

func devMinor(dev uint64) uint32 {
	return uint32(dev&0xff) | uint32((dev>>12)&^0xff)
}
