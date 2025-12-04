//go:build darwin
// +build darwin

package gocam

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc -fmodules
#cgo darwin LDFLAGS: -framework AVFoundation -framework CoreMedia -framework CoreVideo -framework Foundation

#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <stdlib.h>
#import <string.h>

#define GOCAM_CIF_WIDTH  352
#define GOCAM_CIF_HEIGHT 288

static AVCaptureSession *gSession;
static dispatch_queue_t gQueue;
static uint8_t *gFrameBuf;
static int gFrameWidth;
static int gFrameHeight;
static int gFrameReady;
static NSLock *gLock;

static inline uint8_t clampByte(int v) {
    if (v < 0) return 0;
    if (v > 255) return 255;
    return (uint8_t)v;
}

@interface GoFrameDelegate : NSObject<AVCaptureVideoDataOutputSampleBufferDelegate>
@end

@implementation GoFrameDelegate
- (void)captureOutput:(AVCaptureOutput *)output
 didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
        fromConnection:(AVCaptureConnection *)connection
{
    CVImageBufferRef img = CMSampleBufferGetImageBuffer(sampleBuffer);
    if (!img) return;

    CVPixelBufferLockBaseAddress(img, kCVPixelBufferLock_ReadOnly);

    size_t w = CVPixelBufferGetWidth(img);
    size_t h = CVPixelBufferGetHeight(img);
    OSType fmt = CVPixelBufferGetPixelFormatType(img);

    if (w == 0 || h == 0) {
        CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
        return;
    }

    // Determine output size and source crop region.
    // If the source is larger than CIF in at least one dimension, we:
    //   - set destination size to CIF (352x288),
    //   - compute a centered crop in the source that preserves aspect ratio,
    //   - and resample that crop into the CIF canvas ("fill" behavior).
    // If the source is already smaller or equal to CIF, we keep the native size
    // and do not crop or upscale.
    size_t dstW = w;
    size_t dstH = h;
    size_t srcX0 = 0;
    size_t srcY0 = 0;
    size_t srcW = w;
    size_t srcH = h;

    if (w > GOCAM_CIF_WIDTH || h > GOCAM_CIF_HEIGHT) {
        double dstAspect = (double)GOCAM_CIF_WIDTH / (double)GOCAM_CIF_HEIGHT;
        double srcAspect = (double)w / (double)h;

        dstW = GOCAM_CIF_WIDTH;
        dstH = GOCAM_CIF_HEIGHT;

        if (srcAspect > dstAspect) {
            // Source is wider than destination: crop left/right.
            srcH = h;
            srcW = (size_t)((double)h * dstAspect);
            if (srcW > w) srcW = w;
            srcX0 = (w > srcW) ? (w - srcW) / 2 : 0;
            srcY0 = 0;
        } else {
            // Source is taller than destination: crop top/bottom.
            srcW = w;
            srcH = (size_t)((double)w / dstAspect);
            if (srcH > h) srcH = h;
            srcX0 = 0;
            srcY0 = (h > srcH) ? (h - srcH) / 2 : 0;
        }
    }

    // We normalize everything into a tightly packed YCbCr 4:4:4 buffer (packed Y, Cb, Cr) in gFrameBuf.
    size_t bufSize444 = dstW * dstH * 3;

    if (fmt == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange ||
        fmt == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange) {

        if (!CVPixelBufferIsPlanar(img) || CVPixelBufferGetPlaneCount(img) < 2) {
            CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
            return;
        }

        uint8_t *srcY  = (uint8_t *)CVPixelBufferGetBaseAddressOfPlane(img, 0);
        size_t strideY = CVPixelBufferGetBytesPerRowOfPlane(img, 0);

        uint8_t *srcUV  = (uint8_t *)CVPixelBufferGetBaseAddressOfPlane(img, 1);
        size_t strideUV = CVPixelBufferGetBytesPerRowOfPlane(img, 1);

        if (!srcY || !srcUV || strideY == 0 || strideUV == 0) {
            CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
            return;
        }

        [gLock lock];

        if (!gFrameBuf || gFrameWidth != (int)dstW || gFrameHeight != (int)dstH) {
            if (gFrameBuf) {
                free(gFrameBuf);
            }
            gFrameBuf = (uint8_t *)malloc(bufSize444);
            gFrameWidth = (int)dstW;
            gFrameHeight = (int)dstH;
        }

        if (gFrameBuf) {
            uint8_t *dst = gFrameBuf;

            for (size_t dy = 0; dy < dstH; dy++) {
                // Map destination Y to source Y within the cropped region.
                size_t syRel = (dstH > 0) ? (dy * srcH) / dstH : 0;
                size_t sy = srcY0 + syRel;
                if (sy >= h) sy = h - 1;

                uint8_t *rowY  = srcY + sy * strideY;
                uint8_t *rowUV = srcUV + (sy / 2) * strideUV;

                for (size_t dx = 0; dx < dstW; dx++) {
                    size_t sxRel = (dstW > 0) ? (dx * srcW) / dstW : 0;
                    size_t sx = srcX0 + sxRel;
                    if (sx >= w) sx = w - 1;

                    uint8_t Y  = rowY[sx];
                    size_t uvx = (sx / 2) * 2;
                    uint8_t Cb = rowUV[uvx];
                    uint8_t Cr = rowUV[uvx + 1];

                    size_t di = (dy * dstW + dx) * 3;
                    dst[di]     = Y;
                    dst[di + 1] = Cb;
                    dst[di + 2] = Cr;
                }
            }

            gFrameReady = 1;
        }

        [gLock unlock];

        CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
        return;
    }

    if (fmt == kCVPixelFormatType_444YpCbCr8) {
        if (!CVPixelBufferIsPlanar(img) || CVPixelBufferGetPlaneCount(img) < 3) {
            CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
            return;
        }

        uint8_t *srcY  = (uint8_t *)CVPixelBufferGetBaseAddressOfPlane(img, 0);
        size_t strideY = CVPixelBufferGetBytesPerRowOfPlane(img, 0);

        uint8_t *srcCb = (uint8_t *)CVPixelBufferGetBaseAddressOfPlane(img, 1);
        size_t strideCb = CVPixelBufferGetBytesPerRowOfPlane(img, 1);

        uint8_t *srcCr = (uint8_t *)CVPixelBufferGetBaseAddressOfPlane(img, 2);
        size_t strideCr = CVPixelBufferGetBytesPerRowOfPlane(img, 2);

        if (!srcY || !srcCb || !srcCr || strideY == 0 || strideCb == 0 || strideCr == 0) {
            CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
            return;
        }

        [gLock lock];

        if (!gFrameBuf || gFrameWidth != (int)dstW || gFrameHeight != (int)dstH) {
            if (gFrameBuf) {
                free(gFrameBuf);
            }
            gFrameBuf = (uint8_t *)malloc(bufSize444);
            gFrameWidth = (int)dstW;
            gFrameHeight = (int)dstH;
        }

        if (gFrameBuf) {
            uint8_t *dst = gFrameBuf;

            for (size_t dy = 0; dy < dstH; dy++) {
                size_t syRel = (dstH > 0) ? (dy * srcH) / dstH : 0;
                size_t sy = srcY0 + syRel;
                if (sy >= h) sy = h - 1;

                uint8_t *rowY  = srcY  + sy * strideY;
                uint8_t *rowCb = srcCb + sy * strideCb;
                uint8_t *rowCr = srcCr + sy * strideCr;

                for (size_t dx = 0; dx < dstW; dx++) {
                    size_t sxRel = (dstW > 0) ? (dx * srcW) / dstW : 0;
                    size_t sx = srcX0 + sxRel;
                    if (sx >= w) sx = w - 1;

                    uint8_t Y  = rowY[sx];
                    uint8_t Cb = rowCb[sx];
                    uint8_t Cr = rowCr[sx];

                    size_t di = (dy * dstW + dx) * 3;
                    dst[di]     = Y;
                    dst[di + 1] = Cb;
                    dst[di + 2] = Cr;
                }
            }

            gFrameReady = 1;
        }

        [gLock unlock];

        CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
        return;
    }

    if (fmt == kCVPixelFormatType_32BGRA) {
        uint8_t *src = (uint8_t *)CVPixelBufferGetBaseAddress(img);
        size_t stride = CVPixelBufferGetBytesPerRow(img);

        if (!src || stride == 0) {
            CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
            return;
        }

        [gLock lock];

        if (!gFrameBuf || gFrameWidth != (int)dstW || gFrameHeight != (int)dstH) {
            if (gFrameBuf) {
                free(gFrameBuf);
            }
            gFrameBuf = (uint8_t *)malloc(bufSize444);
            gFrameWidth = (int)dstW;
            gFrameHeight = (int)dstH;
        }

        if (gFrameBuf) {
            uint8_t *dst = gFrameBuf;

            for (size_t dy = 0; dy < dstH; dy++) {
                size_t syRel = (dstH > 0) ? (dy * srcH) / dstH : 0;
                size_t sy = srcY0 + syRel;
                if (sy >= h) sy = h - 1;

                uint8_t *row = src + sy * stride;
                for (size_t dx = 0; dx < dstW; dx++) {
                    size_t sxRel = (dstW > 0) ? (dx * srcW) / dstW : 0;
                    size_t sx = srcX0 + sxRel;
                    if (sx >= w) sx = w - 1;

                    size_t si = sx * 4;
                    uint8_t B = row[si + 0];
                    uint8_t G = row[si + 1];
                    uint8_t R = row[si + 2];

                    int iR = (int)R;
                    int iG = (int)G;
                    int iB = (int)B;

                    int Y  = (66 * iR + 129 * iG + 25 * iB + 128) >> 8; Y  += 16;
                    int Cb = (-38 * iR - 74 * iG + 112 * iB + 128) >> 8; Cb += 128;
                    int Cr = (112 * iR - 94 * iG - 18 * iB + 128) >> 8; Cr += 128;

                    size_t di = (dy * dstW + dx) * 3;
                    dst[di]     = clampByte(Y);
                    dst[di + 1] = clampByte(Cb);
                    dst[di + 2] = clampByte(Cr);
                }
            }

            gFrameReady = 1;
        }

        [gLock unlock];

        CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
        return;
    }

    // Unsupported pixel format: just ignore the frame.
    CVPixelBufferUnlockBaseAddress(img, kCVPixelBufferLock_ReadOnly);
}
@end

static GoFrameDelegate *gDelegate;

// StartCapture: 0 ok, <0 error
int StartCapture() {
    @autoreleasepool {
        gLock = [NSLock new];

        AVCaptureDevice *dev = [AVCaptureDevice defaultDeviceWithMediaType:AVMediaTypeVideo];
        if (!dev) return -1;

        NSError *err = nil;
        AVCaptureDeviceInput *input = [AVCaptureDeviceInput deviceInputWithDevice:dev error:&err];
        if (err || !input) return -2;

        AVCaptureSession *session = [[AVCaptureSession alloc] init];
        if (!session) return -3;

        [session beginConfiguration];
        if ([session canSetSessionPreset:AVCaptureSessionPreset352x288]) {
            session.sessionPreset = AVCaptureSessionPreset352x288;
        }
        if (gFrameWidth <= 0 || gFrameHeight <= 0) {
            gFrameWidth = 352;
            gFrameHeight = 288;
        }

        if (![session canAddInput:input]) {
            return -4;
        }
        [session addInput:input];

        AVCaptureVideoDataOutput *out = [[AVCaptureVideoDataOutput alloc] init];

        // Prefer YUV444; fall back to NV12 if the device does not support it.
        OSType chosenFormat = kCVPixelFormatType_420YpCbCr8BiPlanarFullRange;
        NSArray<NSNumber *> *available = out.availableVideoCVPixelFormatTypes;
        for (NSNumber *num in available) {
            OSType f = (OSType)num.unsignedIntValue;
            if (f == kCVPixelFormatType_444YpCbCr8) {
                chosenFormat = kCVPixelFormatType_444YpCbCr8;
                break;
            }
        }

        NSDictionary *settings = @{
            (id)kCVPixelBufferPixelFormatTypeKey : @(chosenFormat)
        };
        out.videoSettings = settings;
        out.alwaysDiscardsLateVideoFrames = YES;

        gDelegate = [GoFrameDelegate new];
        gQueue = dispatch_queue_create("go.av.capture", DISPATCH_QUEUE_SERIAL);
        [out setSampleBufferDelegate:gDelegate queue:gQueue];

        if (![session canAddOutput:out]) {
            return -5;
        }
        [session addOutput:out];

        [session commitConfiguration];
        [session startRunning];

        gSession = session;
    }
    return 0;
}

void StopCapture() {
    @autoreleasepool {
        if (gSession) {
            [gSession stopRunning];
            gSession = nil;
        }
        if (gFrameBuf) {
            free(gFrameBuf);
            gFrameBuf = NULL;
        }
        gFrameWidth = 0;
        gFrameHeight = 0;
        gFrameReady = 0;
        gDelegate = nil;
        gQueue = nil;
        gLock = nil;
    }
}

// GetFrame: 0 ok, -1 no new frame
int GetFrame(uint8_t **buf, int *w, int *h, int *frameSizeOut) {
    if (!gFrameBuf || !gLock) {
        return -1;
    }

    [gLock lock];

    if (!gFrameReady) {
        [gLock unlock];
        return -1;
    }

    *buf = gFrameBuf;
    *w = gFrameWidth;
    *h = gFrameHeight;
    if (frameSizeOut) {
        *frameSizeOut = gFrameWidth * gFrameHeight * 3;
    }
    gFrameReady = 0; // mark as consumed

    [gLock unlock];

    return 0;
}

int GetFrameSize(int *w, int *h) {
    if (!gLock) {
        return -1;
    }

    [gLock lock];
    if (gFrameWidth <= 0 || gFrameHeight <= 0) {
        [gLock unlock];
        return -1;
    }
    if (w) {
        *w = gFrameWidth;
    }
    if (h) {
        *h = gFrameHeight;
    }
    [gLock unlock];

    return 0;
}
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"
	"unsafe"
)

var camLog = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

func logCameraConfig() {
	camLog.Println("[gocam] [AVFoundation]")
	camLog.Println("[gocam]   Camera (index 0) (Capture)")

	var cw, ch C.int
	if C.GetFrameSize(&cw, &ch) != 0 {
		camLog.Println("[gocam]     Resolution:  unknown")
		camLog.Println("[gocam]     Format:      YUV -> YCbCr 4:4:4 (uint8)")
		camLog.Println("[gocam]     Buffer:      unknown")
		camLog.Println("[gocam]     Conversion:")
		camLog.Println("[gocam]       Pre Format Conversion:  device-dependent (NV12/YUV444/BGRA)")
		camLog.Println("[gocam]       Post Format Conversion: YES (to packed YCbCr444)")
		camLog.Println("[gocam]       Resampling:             NO")
		return
	}

	w := int(cw)
	h := int(ch)
	if w <= 0 || h <= 0 {
		camLog.Println("[gocam]     Resolution:  invalid")
		camLog.Println("[gocam]     Format:      YUV -> YCbCr 4:4:4 (uint8)")
		camLog.Println("[gocam]     Buffer:      invalid")
		camLog.Println("[gocam]     Conversion:")
		camLog.Println("[gocam]       Pre Format Conversion:  device-dependent (NV12/YUV444/BGRA)")
		camLog.Println("[gocam]       Post Format Conversion: YES (to packed YCbCr444)")
		camLog.Println("[gocam]       Resampling:             NO")
		return
	}

	bufPixels := w * h
	bufBytes := bufPixels * 3

	camLog.Printf("[gocam]     Resolution:  %d x %d\n", w, h)
	camLog.Printf("[gocam]     Format:      YUV -> YCbCr 4:4:4 (uint8)\n")
	camLog.Printf("[gocam]     Buffer:      %d*3 (%d bytes)\n", bufPixels, bufBytes)
	camLog.Println("[gocam]     Conversion:")
	camLog.Println("[gocam]       Pre Format Conversion:  NO  (already uncompressed)")
	camLog.Println("[gocam]       Post Format Conversion: YES (to packed YCbCr444)")
	camLog.Println("[gocam]       Resampling:             NO")
}

// StartStream starts camera capture and returns a channel with frames encoded
// as tightly packed YCbCr 4:4:4 (YUV444) buffers (3 bytes per pixel, packed Y, Cb, Cr).
// Capture lifetime is controlled by ctx: when the context is canceled, capture stops.
func StartStream(ctx context.Context) (<-chan Frame, error) {
	rc := C.StartCapture()
	if rc != 0 {
		return nil, fmt.Errorf("cannot start capture, rc=%d", int(rc))
	}

	frames := make(chan Frame, 1)

	var loggedResolution atomic.Int64

	logOnce := func() {
		var cw, ch C.int
		if C.GetFrameSize(&cw, &ch) != 0 {
			return
		}
		w := int64(cw)
		h := int64(ch)
		if w <= 0 || h <= 0 {
			return
		}
		newVal := (w << 32) | (h & 0xffffffff)
		if loggedResolution.CompareAndSwap(0, newVal) {
			logCameraConfig()
		}
	}

	go func() {
		defer close(frames)
		defer C.StopCapture()

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

		makeBlack := func(w, h int) []byte {
			size := w * h * 3
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
			var cw, ch C.int
			if C.GetFrameSize(&cw, &ch) != 0 {
				return false
			}
			w := int(cw)
			h := int(ch)
			if w <= 0 || h <= 0 {
				return false
			}
			data := makeBlack(w, h)
			if data == nil {
				return false
			}
			frame := Frame{
				Data:   data,
				Width:  w,
				Height: h,
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

			var cbuf *C.uchar
			var cw, ch C.int
			var csize C.int

			if C.GetFrame(&cbuf, &cw, &ch, &csize) != 0 {
				handleDrop(33*time.Millisecond, 10*time.Millisecond)
				continue
			}

			w := int(cw)
			h := int(ch)
			size := int(csize)
			if w <= 0 || h <= 0 || size <= 0 || cbuf == nil {
				handleDrop(33*time.Millisecond, 5*time.Millisecond)
				continue
			}

			data := C.GoBytes(unsafe.Pointer(cbuf), C.int(size))

			// C side already provides packed YCbCr 4:4:4 (3 bytes per pixel).
			if len(data) != w*h*3 {
				handleDrop(33*time.Millisecond, 5*time.Millisecond)
				continue
			}

			frame := Frame{
				Data:   data,
				Width:  w,
				Height: h,
			}

			logOnce()

			misses = 0
			sendFrame(frame)
		}
	}()

	return frames, nil
}
