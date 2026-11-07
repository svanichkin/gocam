package gocam

import (
	"fmt"
	"strconv"
	"strings"
)

// Helpers for the Plan 9 backend (9front's nusb/cam), kept free of build
// tags so they are tested on every platform.

// nusbFormat is one line of a nusb/cam "formats" file, such as
// "640x480x16-YUY2 30.00,15.00".
type nusbFormat struct {
	Width, Height, Bits int
	FourCC              string
}

func (f nusbFormat) String() string {
	return fmt.Sprintf("%dx%dx%d-%s", f.Width, f.Height, f.Bits, f.FourCC)
}

// parseNusbFormats reads the formats file: one "WxHxBPP-FOURCC fps..." line
// per resolution. Lines it cannot read are skipped.
func parseNusbFormats(s string) []nusbFormat {
	var out []nusbFormat
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		size, fourcc, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		dims := strings.Split(size, "x")
		if len(dims) != 3 {
			continue
		}
		w, err1 := strconv.Atoi(dims[0])
		h, err2 := strconv.Atoi(dims[1])
		b, err3 := strconv.Atoi(dims[2])
		if err1 != nil || err2 != nil || err3 != nil || w <= 0 || h <= 0 {
			continue
		}
		out = append(out, nusbFormat{Width: w, Height: h, Bits: b, FourCC: fourcc})
	}
	return out
}

// pickNusbFormat chooses the YUY2 resolution to stream (the only format
// nusb/cam converts): the smallest that still covers CIF, otherwise the
// largest there is. ok is false when the camera offers no YUY2 at all.
func pickNusbFormat(formats []nusbFormat) (best nusbFormat, ok bool) {
	covers := func(f nusbFormat) bool { return f.Width >= cifWidth && f.Height >= cifHeight }
	for _, f := range formats {
		if f.FourCC != "YUY2" {
			continue
		}
		if !ok {
			best, ok = f, true
			continue
		}
		area, bestArea := f.Width*f.Height, best.Width*best.Height
		switch {
		case covers(f) != covers(best):
			if covers(f) {
				best = f
			}
		case covers(f): // both cover CIF: the smaller
			if area < bestArea {
				best = f
			}
		default: // neither does: the larger
			if area > bestArea {
				best = f
			}
		}
	}
	return best, ok
}

// nusbHeaderSize is the length of the uncompressed image(6) header that
// precedes every frame in nusb/cam's video file.
const nusbHeaderSize = 60

// parseImageHeader reads an image(6) header: channel descriptor and the
// rectangle, five 11-character fields each followed by a space. It returns
// the frame size and the matching gocam pixel format.
func parseImageHeader(h []byte) (w, ht int, pixFmt uint32, err error) {
	if len(h) < nusbHeaderSize {
		return 0, 0, 0, fmt.Errorf("short image header (%d bytes)", len(h))
	}
	fields := strings.Fields(string(h[:nusbHeaderSize]))
	if len(fields) != 5 {
		return 0, 0, 0, fmt.Errorf("bad image header %q", h[:nusbHeaderSize])
	}
	var r [4]int
	for i := range r {
		if r[i], err = strconv.Atoi(fields[i+1]); err != nil {
			return 0, 0, 0, fmt.Errorf("bad image header %q", h[:nusbHeaderSize])
		}
	}
	w, ht = r[2]-r[0], r[3]-r[1]
	if w <= 0 || ht <= 0 {
		return 0, 0, 0, fmt.Errorf("bad image size %dx%d", w, ht)
	}
	// Plan 9 channel descriptors name components from the most significant
	// byte, and pixels are little-endian: b8g8r8 is R, G, B in memory.
	switch fields[0] {
	case "b8g8r8":
		pixFmt = pixFmtRGB24
	case "r8g8b8":
		pixFmt = pixFmtBGR24
	default:
		return 0, 0, 0, fmt.Errorf("unsupported channel %q", fields[0])
	}
	return w, ht, pixFmt, nil
}
