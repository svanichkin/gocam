# Gocam


# Gocam

Cross-platform camera capture for Go with a tiny, unified API.

`gocam` abstracts the native camera stack on each platform:

- **macOS**: AVFoundation (via cgo + Objective‑C)
- **Linux**: V4L2 (`/dev/video*`)
- **Windows**: Media Foundation

You get the same Go API everywhere:

```go
frames, err := gocam.StartStream(ctx)
```

Each frame is packed YCbCr444 (Y, Cb, Cr per pixel). Resolution is CIF (352×288) when the camera outputs larger frames; smaller frames are passed through without upscaling.

---

## Features

- Simple, single entrypoint: `StartStream(ctx)`.
- Common `Frame` struct on all platforms:
  ```go
  type Frame struct {
      Data   []byte // YCbCr444 (packed), len = Width * Height * 3
      Width  int
      Height int
  }
  ```
- Drops old frames if the consumer is slow (keeps only the freshest frame).
- Uses native APIs on each OS, no third-party runtime dependencies.
- Designed as a low-level primitive you can plug into any pipeline (terminal renderer, OpenGL, WebRTC, etc).

---

## Installation

Assuming your module path is:

```txt
github.com/yourname/gocam
```

Add it to your project:

```bash
go get github.com/yourname/gocam
```

Then import:

```go
import "github.com/yourname/gocam"
```

> Replace `github.com/yourname/gocam` with the actual repo path you use on GitHub.

---

## Usage

Minimal example that starts the camera, reads frames, and prints their size:

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/yourname/gocam"
)

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    frames, err := gocam.StartStream(ctx)
    if err != nil {
        panic(err)
    }

    // Simple demo: read a few frames and exit
    for i := 0; i < 10; i++ {
        frame, ok := <-frames
        if !ok {
            fmt.Println("frame stream closed")
            return
        }

        fmt.Printf("Frame %d: %dx%d, %d bytes\n",
            i,
            frame.Width,
            frame.Height,
            len(frame.Data),
        )

        time.Sleep(100 * time.Millisecond)
    }
}
```

From here you can:

- render to a GUI/window,
- feed into an encoder,
- convert to other pixel formats,
- run computer vision / ML on the `Data` bytes.

---

## Platform specifics

### macOS

- Uses **AVFoundation** via cgo.
- Picks the default video device.
- Requirements:
  - Go with cgo enabled.
  - Xcode Command Line Tools (for headers and toolchain).

### Linux

- Uses **V4L2** directly via syscalls.
- Default device: `/dev/video0`.
- Requirements:
  - A V4L2-compatible camera.
  - Access to `/dev/video0` (e.g. user in the `video` group).
- Notes:
  - Implementation accepts several V4L2 pixel formats (YUV24, NV12, YUYV, RGB24) and always converts them into packed YCbCr444.

### Windows

- Uses **Media Foundation**.
- Enumerates video capture devices and opens the first one.
- Requests RGB24 frames via `IMFSourceReader`.
- Requirements:
  - Supported version of Windows with Media Foundation available.
  - cgo enabled (standard Go on Windows with a C toolchain).

---

## Build tags

`gocam` uses per-OS files:

- `capture_macos.go` – `//go:build darwin`
- `capture_linux.go` – `//go:build linux`
- `capture_windows.go` – `//go:build windows`

Go automatically picks the correct implementation for your target OS.
You do not need to add any tags manually for normal use.

---

## API

Current public surface:

```go
// Frame is a single YCbCr444-packed frame from the camera.
type Frame struct {
    Data   []byte // YCbCr444 packed, len(Data) == Width * Height * 3
    Width  int
    Height int
}

// StartStream starts camera capture and returns a channel of frames.
// The context controls the lifetime; cancel it to stop streaming.
//
// Only the latest frame is kept in the buffer. If the consumer is too slow,
// old frames are dropped in favor of the most recent one.
//
// On error (no camera, no permissions, unsupported platform API, etc.),
// StartStream returns a non-nil error.
func StartStream(ctx context.Context) (<-chan Frame, error)
```

This is intentionally minimal and low-level.

---

## Error handling tips

Common failure reasons:

- No camera is available.
- Insufficient permissions:
  - macOS: camera privacy settings.
  - Linux: not in `video` group or no `/dev/video0`.
  - Windows: camera disabled or blocked by privacy settings.
- Platform API initialization issues (Media Foundation / AVFoundation / V4L2).

Always check:

```go
frames, err := gocam.StartStream(ctx)
if err != nil {
    // inspect / log and handle gracefully
}
```

---

## Status

This library is experimental and focused on being:

- small,
- understandable,
- a good base for your own rendering/processing layer.

Contributions and issue reports are welcome.