package gocam

import "time"

type Frame struct {
	Data   []byte
	Width  int
	Height int
	// CapturedAt is when the camera captured the frame, where the system
	// tells (macOS); zero where it does not.
	CapturedAt time.Time
}
