package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	gocam "github.com/svanichkin/gocam"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("signal received, stopping stream")
		cancel()
	}()

	frames, err := gocam.StartStream(ctx)
	if err != nil {
		log.Fatalf("gocam: %v", err)
	}
	log.Println("camera stream started")

	var lastFrame gocam.Frame
	const logCount = 5
	for i := 0; i < logCount; {
		select {
		case <-ctx.Done():
			log.Fatalf("gocam: context canceled: %v", ctx.Err())
		case frame, ok := <-frames:
			if !ok {
				log.Fatal("gocam: frame stream closed")
			}
			lastFrame = frame
			log.Printf("frame %d: %dx%d (%d bytes)", i+1, frame.Width, frame.Height, len(frame.Data))
			i++
		}
	}

	cancel()
	time.Sleep(200 * time.Millisecond)

	if lastFrame.Width == 0 || lastFrame.Height == 0 {
		log.Fatal("gocam: no frame captured for snapshot")
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("gocam snapshot: %v", err)
	}
	outputPath := filepath.Join(wd, "snapshot.png")

	if err := gocam.SaveFramePNG(lastFrame, outputPath); err != nil {
		log.Fatalf("gocam snapshot: %v", err)
	}

	log.Printf("snapshot saved to %s", outputPath)
}
