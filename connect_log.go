package gocam

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	frameLogCount   = 5
	frameLogTimeout = 2 * time.Second
)

// ConnectAndLog starts the camera stream and logs a handful of frames for smoke testing.
func ConnectAndLog(ctx context.Context, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}

	frames, err := StartStream(ctx)
	if err != nil {
		return fmt.Errorf("start camera stream: %w", err)
	}

	logger.Println("camera stream started")

	for i := 0; i < frameLogCount; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return errors.New("frame stream closed")
			}
			logger.Printf("frame %d: %dx%d (%d bytes)", i+1, frame.Width, frame.Height, len(frame.Data))
		case <-time.After(frameLogTimeout):
			logger.Println("no frame received within timeout")
		}
	}

	return nil
}
