//go:build plan9

package gocam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Plan 9 backend: 9front's USB video driver, nusb/cam.
//
// nusb/cam serves a directory camN.M per camera under /shr/usb, which
// nusbrc binds after /dev. Its files: formats (the resolutions), ctl
// (settings, "format WxH..." among them), and video, a stream of
// uncompressed image(6) frames: a 60-byte header, then RGB pixels converted
// by the driver from YUY2 (its only supported camera format).
//
// Finding a camera: GOCAM_DEVICE (a camN.M directory) if set, otherwise
// every /dev/cam* and /shr/usb/cam* directory in name order. Before the
// stream opens, the smallest YUY2 resolution covering CIF is selected.

var camLog = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

// StartStream finds a camera, starts its video stream, and returns a channel
// of frames encoded as tightly packed YCbCr 4:4:4 buffers.
func StartStream(ctx context.Context) (<-chan Frame, error) {
	dirs := candidateCams()
	if len(dirs) == 0 {
		return nil, errors.New("gocam: no camera under /dev/cam* or /shr/usb/cam* (is nusb/cam running?)")
	}
	var failures []string
	for _, dir := range dirs {
		f, err := openCam(dir)
		if err != nil {
			camLog.Printf("[gocam] %s: %v", dir, err)
			failures = append(failures, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		return runCam(ctx, dir, f), nil
	}
	return nil, fmt.Errorf("gocam: no usable camera (%s)", strings.Join(failures, "; "))
}

// candidateCams lists GOCAM_DEVICE alone if set, otherwise the camera
// directories nusb/cam serves, each name once.
func candidateCams() []string {
	if dev := strings.TrimSpace(os.Getenv("GOCAM_DEVICE")); dev != "" {
		return []string{dev}
	}
	seen := map[string]bool{}
	var dirs []string
	for _, pattern := range []string{"/dev/cam*", "/shr/usb/cam*"} {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		for _, m := range matches {
			name := filepath.Base(m)
			if seen[name] {
				continue
			}
			if _, err := os.Stat(filepath.Join(m, "video")); err != nil {
				continue
			}
			seen[name] = true
			dirs = append(dirs, m)
		}
	}
	return dirs
}

// openCam selects the resolution and opens the video stream.
func openCam(dir string) (*os.File, error) {
	formats, err := os.ReadFile(filepath.Join(dir, "formats"))
	if err != nil {
		return nil, err
	}
	pick, ok := pickNusbFormat(parseNusbFormats(string(formats)))
	if !ok {
		return nil, fmt.Errorf("no YUY2 format (offers: %s)", strings.Join(strings.Fields(string(formats)), " "))
	}
	if err := writeCtl(dir, "format "+pick.String()); err != nil {
		// The camera stays at its current format; opening video tells
		// whether the driver can use it.
		camLog.Printf("[gocam] %s: format %s: %v", dir, pick, err)
	}
	f, err := os.Open(filepath.Join(dir, "video"))
	if err != nil {
		return nil, err
	}
	camLog.Println("[gocam] [nusb/cam]")
	camLog.Printf("[gocam]   %s\n", dir)
	camLog.Printf("[gocam]     Format:      %s -> YCbCr 4:4:4\n", pick)
	return f, nil
}

func writeCtl(dir, msg string) error {
	ctl, err := os.OpenFile(filepath.Join(dir, "ctl"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer ctl.Close()
	_, err = ctl.Write([]byte(msg))
	return err
}

// runCam reads frames until ctx is done; it owns and finally closes f.
// After about 300 ms without a frame, black frames stand in for the camera.
func runCam(ctx context.Context, dir string, f *os.File) <-chan Frame {
	frames := make(chan Frame, 1)
	raw := make(chan Frame, 1)

	// Reads block until the driver has a frame, so they run on their own.
	go func() {
		defer close(raw)
		var header [nusbHeaderSize]byte
		var buf []byte
		for {
			if _, err := io.ReadFull(f, header[:]); err != nil {
				if ctx.Err() == nil {
					camLog.Printf("[gocam] %s: read: %v", dir, err)
				}
				return
			}
			w, h, pixFmt, err := parseImageHeader(header[:])
			if err != nil {
				camLog.Printf("[gocam] %s: %v", dir, err)
				return
			}
			if n := w * h * 3; cap(buf) < n {
				buf = make([]byte, n)
			} else {
				buf = buf[:n]
			}
			if _, err := io.ReadFull(f, buf); err != nil {
				if ctx.Err() == nil {
					camLog.Printf("[gocam] %s: read: %v", dir, err)
				}
				return
			}
			data, outW, outH := sampleYCbCr444(buf, pixFmt, w, h, w*3)
			if data == nil {
				continue
			}
			select {
			case raw <- Frame{Data: data, Width: outW, Height: outH}:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		defer close(frames)
		// Closing ends the stream once the blocked read returns.
		defer f.Close()

		sendFrame := func(frame Frame) {
			select {
			case frames <- frame:
			default:
				<-frames
				frames <- frame
			}
		}
		const waitStep = 100 * time.Millisecond
		const dropThreshold = 3
		var black Frame
		misses := 0
		tick := time.NewTicker(waitStep)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-raw:
				if !ok {
					return
				}
				misses = 0
				black = Frame{Width: frame.Width, Height: frame.Height}
				sendFrame(frame)
			case <-tick.C:
				misses++
				if misses >= dropThreshold && black.Width > 0 {
					buf := make([]byte, black.Width*black.Height*3)
					for i := 0; i < len(buf); i += 3 {
						buf[i], buf[i+1], buf[i+2] = 16, 128, 128
					}
					sendFrame(Frame{Data: buf, Width: black.Width, Height: black.Height})
				}
			}
		}
	}()
	return frames
}
