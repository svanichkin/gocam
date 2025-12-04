package gocam

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"time"
)

// CaptureSingleFrame starts the camera, waits for one frame, and returns it.
// A derived context ensures the underlying stream stops once the function returns.
func CaptureSingleFrame(ctx context.Context, timeout time.Duration) (Frame, error) {
	ctxCapture, cancel := context.WithCancel(ctx)
	defer cancel()

	frames, err := StartStream(ctxCapture)
	if err != nil {
		return Frame{}, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctxCapture.Done():
		return Frame{}, ctxCapture.Err()
	case <-timer.C:
		return Frame{}, errors.New("gocam: capture timeout")
	case frame, ok := <-frames:
		if !ok {
			return Frame{}, errors.New("gocam: frame stream closed")
		}
		return frame, nil
	}
}

// SaveFramePNG encodes the provided frame into a PNG file at the given path.
func SaveFramePNG(frame Frame, path string) error {
	if frame.Width <= 0 || frame.Height <= 0 || len(frame.Data) != frame.Width*frame.Height*3 {
		return errors.New("gocam: invalid frame data")
	}

	img := image.NewNRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			idx := (y*frame.Width + x) * 3
			yVal := frame.Data[idx]
			cb := frame.Data[idx+1]
			cr := frame.Data[idx+2]
			r, g, b := color.YCbCrToRGB(yVal, cb, cr)

			di := img.PixOffset(x, y)
			img.Pix[di+0] = r
			img.Pix[di+1] = g
			img.Pix[di+2] = b
			img.Pix[di+3] = 0xff
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return png.Encode(f, img)
}
