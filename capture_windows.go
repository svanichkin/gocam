//go:build windows
// +build windows

package gocam

/*
#cgo windows CFLAGS: -DUNICODE -D_UNICODE
#cgo windows LDFLAGS: -lole32 -lmfplat -lmf -lmfreadwrite -lmfuuid

#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mfobjects.h>
#include <mferror.h>
#include <stdio.h>
#include <stdlib.h>

#include <string.h>

#ifdef __MINGW32__
// MinGW can miss some newer Media Foundation helpers; provide compatible versions.
// We accept a generic pointer here (IMFAttributes* or IMFMediaType*) and cast to IMFAttributes.
static HRESULT MFSetAttributeSizeCompat(void *obj, REFGUID guidKey, UINT32 width, UINT32 height) {
    UINT64 v = ((UINT64)width << 32) | (UINT64)height;
    IMFAttributes *attr = (IMFAttributes *)obj;
    return attr->lpVtbl->SetUINT64(attr, guidKey, v);
}
#define MFSetAttributeSize MFSetAttributeSizeCompat
#endif

static HRESULT GetFrameSizeC(IMFMediaType *type, UINT32 *w, UINT32 *h) {
    UINT64 v = 0;
    HRESULT hr = type->lpVtbl->GetUINT64(type, &MF_MT_FRAME_SIZE, &v);
    if (FAILED(hr)) return hr;
    *w = (UINT32)(v >> 32);
    *h = (UINT32)(v & 0xFFFFFFFF);
    return S_OK;
}

static IMFSourceReader *gReader = NULL;
static CRITICAL_SECTION gLock;
static int gLockInit = 0;

static BYTE *gBuf = NULL;
static LONG gW = 0;      // source width
static LONG gH = 0;      // source height
static LONG gDstW = 0;   // output (resampled) width
static LONG gDstH = 0;   // output (resampled) height
static int gReady = 0;
static int gBufSize = 0;
static int gIsNV12 = 0;
static int gIsYUY2 = 0;
static int gIsRGB32 = 0;
static int gIsUYVY = 0;
static int gIsRGB24 = 0;
static LONG gStrideY = 0;
static LONG gStrideUV = 0;
static char gSubtypeName[32] = "unknown";

// Target CIF resolution
#define GOCAM_CIF_WIDTH  352
#define GOCAM_CIF_HEIGHT 288

static void gcam_init_lock() {
	if (!gLockInit) {
		InitializeCriticalSection(&gLock);
		gLockInit = 1;
	}
}

static void gcam_reset_format_info() {
	gIsNV12 = 0;
	gIsYUY2 = 0;
	gIsRGB32 = 0;
	gIsUYVY = 0;
	gIsRGB24 = 0;
	gStrideY = 0;
	gStrideUV = 0;
	strcpy(gSubtypeName, "unknown");
	// Do not reset gW/gH here; they are source dimensions.
	// Reset output size separately.
	gDstW = 0;
	gDstH = 0;
}

static void gcam_set_format_info(IMFMediaType *type) {
	if (!type) {
		gcam_reset_format_info();
		return;
	}

	GUID subtype;
	if (SUCCEEDED(type->lpVtbl->GetGUID(type, &MF_MT_SUBTYPE, &subtype))) {
		gIsNV12 = 0;
		gIsYUY2 = 0;
		gIsRGB32 = 0;
		gIsUYVY = 0;
		gIsRGB24 = 0;
		if (IsEqualGUID(&subtype, &MFVideoFormat_NV12)) {
			gIsNV12 = 1;
			strcpy(gSubtypeName, "NV12");
		} else if (IsEqualGUID(&subtype, &MFVideoFormat_YUY2)) {
			gIsYUY2 = 1;
			strcpy(gSubtypeName, "YUY2");
		} else if (IsEqualGUID(&subtype, &MFVideoFormat_UYVY)) {
			gIsUYVY = 1;
			strcpy(gSubtypeName, "UYVY");
		} else if (IsEqualGUID(&subtype, &MFVideoFormat_MJPG)) {
			// We expect Media Foundation to decode MJPG to an uncompressed format
			strcpy(gSubtypeName, "MJPG");
		} else if (IsEqualGUID(&subtype, &MFVideoFormat_RGB32)) {
			gIsRGB32 = 1;
			strcpy(gSubtypeName, "RGB32");
		} else if (IsEqualGUID(&subtype, &MFVideoFormat_RGB24)) {
			gIsRGB24 = 1;
			strcpy(gSubtypeName, "RGB24");
		} else {
			snprintf(gSubtypeName, sizeof(gSubtypeName), "%08X", (unsigned int)subtype.Data1);
		}
	} else {
		strcpy(gSubtypeName, "unknown");
	}

	UINT32 stride = 0;
	if (FAILED(type->lpVtbl->GetUINT32(type, &MF_MT_DEFAULT_STRIDE, &stride)) || stride == 0) {
		stride = (UINT32)gW;
	}
	gStrideY = (LONG)stride;
	gStrideUV = (LONG)stride;
}

int gcam_get_format_info(int *isNV12, int *strideY, int *strideUV, char *subtypeBuf, int bufLen) {
	if (isNV12) {
		// Non-zero means: we have a built-in conversion to YCbCr444
		*isNV12 = (gIsNV12 || gIsYUY2 || gIsRGB32 || gIsUYVY || gIsRGB24) ? 1 : 0;
	}
	if (strideY) {
		*strideY = (int)gStrideY;
	}
	if (strideUV) {
		*strideUV = (int)gStrideUV;
	}
	if (subtypeBuf && bufLen > 0) {
		strncpy(subtypeBuf, gSubtypeName, bufLen - 1);
		subtypeBuf[bufLen - 1] = '\0';
	}
	return 0;
}

// StartCapture initializes Media Foundation, selects the first available camera
// and configures an IMFSourceReader for video capture.
HRESULT StartCapture() {
	HRESULT hr;

	// COM + MF
	hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
	if (FAILED(hr) && hr != RPC_E_CHANGED_MODE) {
		return hr;
	}

	hr = MFStartup(MF_VERSION, MFSTARTUP_FULL);
	if (FAILED(hr)) {
		CoUninitialize();
		return hr;
	}

	gcam_init_lock();
	gcam_reset_format_info();

	IMFAttributes *attr = NULL;
	IMFActivate **devices = NULL;
	UINT32 count = 0;

	hr = MFCreateAttributes(&attr, 1);
	if (FAILED(hr)) goto fail;

	hr = attr->lpVtbl->SetGUID(attr, &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE, &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
	if (FAILED(hr)) goto fail;

	hr = MFEnumDeviceSources(attr, &devices, &count);
	if (FAILED(hr) || count == 0) {
		hr = E_FAIL;
		goto fail;
	}

	IMFMediaSource *source = NULL;
	hr = devices[0]->lpVtbl->ActivateObject(devices[0], &IID_IMFMediaSource, (void**)&source);
	if (FAILED(hr)) goto fail;

	hr = MFCreateSourceReaderFromMediaSource(source, NULL, &gReader);
	if (FAILED(hr)) {
		source->lpVtbl->Release(source);
		goto fail;
	}

	// Request an uncompressed output format so MF decodes MJPG and others for us
	// and ask for a CIF frame size. MF may still negotiate a different size, which
	// we will read back from the media type.
	IMFMediaType *type = NULL;
	hr = MFCreateMediaType(&type);
	if (FAILED(hr)) goto fail;

	hr = type->lpVtbl->SetGUID(type, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
	if (FAILED(hr)) goto fail;

	// Try NV12, then YUY2, RGB32, UYVY, RGB24 as output formats.
	GUID desiredSubtypes[5] = {
		MFVideoFormat_NV12,
		MFVideoFormat_YUY2,
		MFVideoFormat_RGB32,
		MFVideoFormat_UYVY,
		MFVideoFormat_RGB24,
	};
	HRESULT setHr = E_FAIL;
	for (int i = 0; i < 5; i++) {
		hr = type->lpVtbl->SetGUID(type, &MF_MT_SUBTYPE, &desiredSubtypes[i]);
		if (FAILED(hr)) {
			continue;
		}

		// Ask MF for CIF resolution. MF is free to negotiate something else,
		// and we will read back the actual size below.
		hr = MFSetAttributeSize(type, &MF_MT_FRAME_SIZE, GOCAM_CIF_WIDTH, GOCAM_CIF_HEIGHT);
		if (FAILED(hr)) {
			continue;
		}

		setHr = gReader->lpVtbl->SetCurrentMediaType(
			gReader,
			MF_SOURCE_READER_FIRST_VIDEO_STREAM,
			NULL,
			type);
		if (SUCCEEDED(setHr)) {
			break;
		}
	}

	if (FAILED(setHr)) {
		type->lpVtbl->Release(type);
		type = NULL;
#ifdef __MINGW32__
		// MinGW: GetCurrentMediaType(this, streamIndex, &type)
		hr = gReader->lpVtbl->GetCurrentMediaType(gReader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, &type);
#else
		// MSVC/SDK: GetCurrentMediaType(this, streamIndex, reserved, &type)
		hr = gReader->lpVtbl->GetCurrentMediaType(gReader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, NULL, &type);
#endif
		if (FAILED(hr)) goto fail;
	}

	// Try to read actual frame size from media type
	UINT32 w = 0, h = 0;
	hr = GetFrameSizeC(type, &w, &h);
	if (SUCCEEDED(hr) && w > 0 && h > 0) {
		gW = (LONG)w;
		gH = (LONG)h;
	} else {
		// Fallback source size if metadata is missing (treat as CIF)
		gW = GOCAM_CIF_WIDTH;
		gH = GOCAM_CIF_HEIGHT;
	}

	// Set desired output size:
	// - If the source is larger than CIF in at least one dimension, downsample to CIF.
	// - Otherwise, keep the native size (no upscaling).
	if (gW > GOCAM_CIF_WIDTH || gH > GOCAM_CIF_HEIGHT) {
		gDstW = GOCAM_CIF_WIDTH;
		gDstH = GOCAM_CIF_HEIGHT;
	} else {
		gDstW = gW;
		gDstH = gH;
	}

	gcam_set_format_info(type);

	type->lpVtbl->Release(type);
	source->lpVtbl->Release(source);
	attr->lpVtbl->Release(attr);

	for (UINT32 i = 0; i < count; i++) {
		devices[i]->lpVtbl->Release(devices[i]);
	}
	CoTaskMemFree(devices);

	return S_OK;

fail:
	if (gReader) {
		gReader->lpVtbl->Release(gReader);
		gReader = NULL;
	}

	if (devices) {
		for (UINT32 i = 0; i < count; i++) {
			if (devices[i]) devices[i]->lpVtbl->Release(devices[i]);
		}
		CoTaskMemFree(devices);
	}
	if (attr) attr->lpVtbl->Release(attr);

	MFShutdown();
	CoUninitialize();

	return hr;
}

static void gcam_free_buf(int resetDims) {
	if (gBuf) {
		free(gBuf);
		gBuf = NULL;
		gBufSize = 0;
	}
	if (resetDims) {
		gW = 0;
		gH = 0;
	}
	gReady = 0;
}

int GetFrameSize(int *w, int *h) {
	if (!gLockInit || gW <= 0 || gH <= 0) {
		return -1;
	}

	EnterCriticalSection(&gLock);
	LONG outW = gDstW > 0 ? gDstW : gW;
	LONG outH = gDstH > 0 ? gDstH : gH;
	if (w) {
		*w = (int)outW;
	}
	if (h) {
		*h = (int)outH;
	}
	LeaveCriticalSection(&gLock);

	return 0;
}

// GetFrame returns 0 on success, -1 if no new frame is available
int GetFrame(unsigned char **buf, int *w, int *h, int *frameSizeOut) {
	if (!gReader || !gLockInit) {
		return -1;
	}

	HRESULT hr;
	DWORD flags = 0;
	IMFSample *sample = NULL;

	hr = gReader->lpVtbl->ReadSample(gReader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, NULL, &flags, NULL, &sample);
	if (FAILED(hr) || !sample) {
		return -1;
	}

	IMFMediaBuffer *mbuf = NULL;
	hr = sample->lpVtbl->ConvertToContiguousBuffer(sample, &mbuf);
	if (FAILED(hr) || !mbuf) {
		if (sample) sample->lpVtbl->Release(sample);
		return -1;
	}

	BYTE *data = NULL;
	DWORD len = 0;
	if (FAILED(mbuf->lpVtbl->Lock(mbuf, &data, NULL, &len)) || !data || len == 0) {
		mbuf->lpVtbl->Release(mbuf);
		sample->lpVtbl->Release(sample);
		return -1;
	}

	int srcW = (int)gW;
	int srcH = (int)gH;
	if (srcW <= 0 || srcH <= 0) {
		mbuf->lpVtbl->Unlock(mbuf);
		mbuf->lpVtbl->Release(mbuf);
		sample->lpVtbl->Release(sample);
		return -1;
	}

	int dstW = (int)(gDstW > 0 ? gDstW : gW);
	int dstH = (int)(gDstH > 0 ? gDstH : gH);
	if (dstW <= 0 || dstH <= 0) {
		mbuf->lpVtbl->Unlock(mbuf);
		mbuf->lpVtbl->Release(mbuf);
		sample->lpVtbl->Release(sample);
		return -1;
	}

	// Define source crop region for aspect-ratio-preserving downsampling.
	int srcX0 = 0;
	int srcY0 = 0;
	int cropW = srcW;
	int cropH = srcH;
	if ((dstW < srcW) || (dstH < srcH)) {
		// We are downsampling. Compute a centered crop that matches the destination aspect ratio.
		double dstAspect = (double)dstW / (double)dstH;
		double srcAspect = (double)srcW / (double)srcH;
		if (srcAspect > dstAspect) {
			// Source is wider than destination: crop left/right.
			cropH = srcH;
			cropW = (int)((double)srcH * dstAspect);
			if (cropW > srcW) cropW = srcW;
			if (cropW < 1) cropW = 1;
			srcX0 = (srcW - cropW) / 2;
			srcY0 = 0;
		} else {
			// Source is taller than destination: crop top/bottom.
			cropW = srcW;
			cropH = (int)((double)srcW / dstAspect);
			if (cropH > srcH) cropH = srcH;
			if (cropH < 1) cropH = 1;
			srcX0 = 0;
			srcY0 = (srcH - cropH) / 2;
		}
	}

	int yStride = (int)gStrideY;
	if (yStride <= 0) {
		yStride = srcW;
	}
	int yPlaneSize = yStride * srcH;
	int uvStride = (int)gStrideUV;
	if (uvStride <= 0) {
		uvStride = yStride;
	}
	int uvPlaneSize = uvStride * (srcH / 2);

	int dstSize = dstW * dstH * 3;
	if (dstSize <= 0) {
		mbuf->lpVtbl->Unlock(mbuf);
		mbuf->lpVtbl->Release(mbuf);
		sample->lpVtbl->Release(sample);
		return -1;
	}

	EnterCriticalSection(&gLock);

	if (!gBuf || gBufSize != dstSize) {
		gcam_free_buf(0);
		gBuf = (BYTE*)malloc(dstSize);
		gBufSize = dstSize;
	}

	if (gBuf) {
		if (gIsNV12) {
			// NV12: Y plane + interleaved UV plane
			int totalNeeded = yPlaneSize + uvPlaneSize;
			if ((int)len < totalNeeded) {
				// Not enough data for NV12 frame
				gReady = 0;
			} else {
				BYTE *yPlane = data;
				BYTE *uvPlane = data + yPlaneSize;

				for (int dy = 0; dy < dstH; dy++) {
					int syRel = (dstH > 0) ? (dy * cropH) / dstH : 0;
					int sy = srcY0 + syRel;
					if (sy >= srcH) sy = srcH - 1;

					for (int dx = 0; dx < dstW; dx++) {
						int sxRel = (dstW > 0) ? (dx * cropW) / dstW : 0;
						int sx = srcX0 + sxRel;
						if (sx >= srcW) sx = srcW - 1;

						int yi = sy * yStride + sx;
						BYTE Y = yPlane[yi];

						int uvY = sy / 2;
						int uvX = (sx / 2) * 2;
						int uvIdx = uvY * uvStride + uvX;
						if (uvIdx + 1 >= uvPlaneSize) {
							continue;
						}
						BYTE Cb = uvPlane[uvIdx];
						BYTE Cr = uvPlane[uvIdx + 1];

						int di = (dy * dstW + dx) * 3;
						gBuf[di]     = Y;
						gBuf[di + 1] = Cb;
						gBuf[di + 2] = Cr;
					}
				}
				gReady = 1;
			}
		} else if (gIsYUY2) {
			// YUY2: packed Y0 U0 Y1 V0 per 4 bytes, 2 pixels
			int minNeeded = yStride * srcH;
			if ((int)len < minNeeded) {
				// Not enough data for YUY2 frame
				gReady = 0;
			} else {
				for (int dy = 0; dy < dstH; dy++) {
					int syRel = (dstH > 0) ? (dy * cropH) / dstH : 0;
					int sy = srcY0 + syRel;
					if (sy >= srcH) sy = srcH - 1;
					BYTE *row = data + sy * yStride;
					for (int dx = 0; dx < dstW; dx++) {
						int sxRel = (dstW > 0) ? (dx * cropW) / dstW : 0;
						int sx = srcX0 + sxRel;
						if (sx >= srcW) sx = srcW - 1;
						int pairIndex = (sx / 2) * 4;
						BYTE Y0 = row[pairIndex + 0];
						BYTE U  = row[pairIndex + 1];
						BYTE Y1 = row[pairIndex + 2];
						BYTE V  = row[pairIndex + 3];

						BYTE Y;
						if ((sx & 1) == 0) {
							Y = Y0;
						} else {
							Y = Y1;
						}

						int di = (dy * dstW + dx) * 3;
						gBuf[di]     = Y;
						gBuf[di + 1] = U;
						gBuf[di + 2] = V;
					}
				}
				gReady = 1;
			}
		} else if (gIsUYVY) {
			// UYVY: packed U0 Y0 V0 Y1 per 4 bytes, 2 pixels
			int minNeeded = yStride * srcH;
			if ((int)len < minNeeded) {
				gReady = 0;
			} else {
				for (int dy = 0; dy < dstH; dy++) {
					int syRel = (dstH > 0) ? (dy * cropH) / dstH : 0;
					int sy = srcY0 + syRel;
					if (sy >= srcH) sy = srcH - 1;
					BYTE *row = data + sy * yStride;
					for (int dx = 0; dx < dstW; dx++) {
						int sxRel = (dstW > 0) ? (dx * cropW) / dstW : 0;
						int sx = srcX0 + sxRel;
						if (sx >= srcW) sx = srcW - 1;
						int pairIndex = (sx / 2) * 4;
						BYTE U  = row[pairIndex + 0];
						BYTE Y0 = row[pairIndex + 1];
						BYTE V  = row[pairIndex + 2];
						BYTE Y1 = row[pairIndex + 3];

						BYTE Y;
						if ((sx & 1) == 0) {
							Y = Y0;
						} else {
							Y = Y1;
						}

						int di = (dy * dstW + dx) * 3;
						gBuf[di]     = Y;
						gBuf[di + 1] = U;
						gBuf[di + 2] = V;
					}
				}
				gReady = 1;
			}
		} else if (gIsRGB32) {
			// RGB32: 4 bytes per pixel (usually BGRA). Convert to YCbCr444.
			int minNeeded = yStride * srcH;
			if ((int)len < minNeeded) {
				gReady = 0;
			} else {
				for (int dy = 0; dy < dstH; dy++) {
					int syRel = (dstH > 0) ? (dy * cropH) / dstH : 0;
					int sy = srcY0 + syRel;
					if (sy >= srcH) sy = srcH - 1;
					BYTE *row = data + sy * yStride;
					for (int dx = 0; dx < dstW; dx++) {
						int sxRel = (dstW > 0) ? (dx * cropW) / dstW : 0;
						int sx = srcX0 + sxRel;
						if (sx >= srcW) sx = srcW - 1;
						int srcIndex = sx * 4;
						BYTE B = row[srcIndex + 0];
						BYTE G = row[srcIndex + 1];
						BYTE R = row[srcIndex + 2];
						// Ignore alpha row[srcIndex + 3]

						// Simple BT.601-ish integer conversion to YCbCr
						int Y  = (  66 * R + 129 * G +  25 * B + 128) >> 8;
						int Cb = ( -38 * R -  74 * G + 112 * B + 128) >> 8;
						int Cr = ( 112 * R -  94 * G -  18 * B + 128) >> 8;

						Y  += 16;
						Cb += 128;
						Cr += 128;

						if (Y   < 0)   Y = 0;   else if (Y   > 255) Y = 255;
						if (Cb  < 0)   Cb = 0;  else if (Cb  > 255) Cb = 255;
						if (Cr  < 0)   Cr = 0;  else if (Cr  > 255) Cr = 255;

						int di = (dy * dstW + dx) * 3;
						gBuf[di]     = (BYTE)Y;
						gBuf[di + 1] = (BYTE)Cb;
						gBuf[di + 2] = (BYTE)Cr;
					}
				}
				gReady = 1;
			}
		} else if (gIsRGB24) {
			// RGB24: 3 bytes per pixel (usually BGR). Convert to YCbCr444.
			int minNeeded = yStride * srcH;
			if ((int)len < minNeeded) {
				gReady = 0;
			} else {
				for (int dy = 0; dy < dstH; dy++) {
					int syRel = (dstH > 0) ? (dy * cropH) / dstH : 0;
					int sy = srcY0 + syRel;
					if (sy >= srcH) sy = srcH - 1;
					BYTE *row = data + sy * yStride;
					for (int dx = 0; dx < dstW; dx++) {
						int sxRel = (dstW > 0) ? (dx * cropW) / dstW : 0;
						int sx = srcX0 + sxRel;
						if (sx >= srcW) sx = srcW - 1;
						int srcIndex = sx * 3;
						BYTE B = row[srcIndex + 0];
						BYTE G = row[srcIndex + 1];
						BYTE R = row[srcIndex + 2];

						// Simple BT.601-ish integer conversion to YCbCr
						int Y  = (  66 * R + 129 * G +  25 * B + 128) >> 8;
						int Cb = ( -38 * R -  74 * G + 112 * B + 128) >> 8;
						int Cr = ( 112 * R -  94 * G -  18 * B + 128) >> 8;

						Y  += 16;
						Cb += 128;
						Cr += 128;

						if (Y   < 0)   Y = 0;   else if (Y   > 255) Y = 255;
						if (Cb  < 0)   Cb = 0;  else if (Cb  > 255) Cb = 255;
						if (Cr  < 0)   Cr = 0;  else if (Cr  > 255) Cr = 255;

						int di = (dy * dstW + dx) * 3;
						gBuf[di]     = (BYTE)Y;
						gBuf[di + 1] = (BYTE)Cb;
						gBuf[di + 2] = (BYTE)Cr;
					}
				}
				gReady = 1;
			}
		} else {
			// Unsupported / exotic format: output black frame in YCbCr444.
			for (int i = 0; i < dstSize; i += 3) {
				gBuf[i]     = 16;   // Y
				gBuf[i + 1] = 128;  // Cb
				gBuf[i + 2] = 128;  // Cr
			}
			gReady = 1;
		}

		if (gReady) {
			*buf = gBuf;
			*w = (int)dstW;
			*h = (int)dstH;
			if (frameSizeOut) {
				*frameSizeOut = dstSize;
			}
		} else {
			*buf = NULL;
			if (frameSizeOut) {
				*frameSizeOut = 0;
			}
		}
	} else {
		gReady = 0;
		*buf = NULL;
		if (frameSizeOut) {
			*frameSizeOut = 0;
		}
	}

	LeaveCriticalSection(&gLock);

	mbuf->lpVtbl->Unlock(mbuf);

	mbuf->lpVtbl->Release(mbuf);
	sample->lpVtbl->Release(sample);

	return gReady ? 0 : -1;
}

void StopCapture() {
	if (gLockInit) {
		EnterCriticalSection(&gLock);
	}

	if (gReader) {
		gReader->lpVtbl->Release(gReader);
		gReader = NULL;
	}

	gcam_free_buf(1);
	gcam_reset_format_info();

	if (gLockInit) {
		LeaveCriticalSection(&gLock);
		DeleteCriticalSection(&gLock);
		gLockInit = 0;
	}

	MFShutdown();
	CoUninitialize();
}
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
	"unsafe"
)

var camLog = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

// logCameraConfig prints a human-readable description of the current camera configuration.
func logCameraConfig() {
	var cw, ch C.int
	if C.GetFrameSize(&cw, &ch) != 0 {
		camLog.Println("[gocam] [MediaFoundation]")
		camLog.Println("[gocam]   Camera (index 0) (Capture)")
		camLog.Println("[gocam]     Resolution:  unknown")
		camLog.Println("[gocam]     Format:      NV12 -> YCbCr 4:4:4 (uint8)")
		return
	}

	w := int(cw)
	h := int(ch)
	if w <= 0 || h <= 0 {
		camLog.Println("[gocam] [MediaFoundation]")
		camLog.Println("[gocam]   Camera (index 0) (Capture)")
		camLog.Println("[gocam]     Resolution:  invalid")
		camLog.Println("[gocam]     Format:      NV12 -> YCbCr 4:4:4 (uint8)")
		return
	}

	bufPixels := w * h
	bufBytes := bufPixels * 3

	camLog.Println("[gocam] [MediaFoundation]")
	camLog.Println("[gocam]   Camera (index 0) (Capture)")

	var isNV12 C.int
	var strideY C.int
	var strideUV C.int
	nameBuf := make([]byte, 32)
	C.gcam_get_format_info(
		(*C.int)(unsafe.Pointer(&isNV12)),
		(*C.int)(unsafe.Pointer(&strideY)),
		(*C.int)(unsafe.Pointer(&strideUV)),
		(*C.char)(unsafe.Pointer(&nameBuf[0])),
		C.int(len(nameBuf)),
	)

	formatName := C.GoString((*C.char)(unsafe.Pointer(&nameBuf[0])))
	if formatName == "" {
		formatName = "unknown"
	}
	if isNV12 != 0 {
		camLog.Printf("[gocam]     Format:      %s -> YCbCr 4:4:4 (uint8)\n", formatName)
	} else {
		camLog.Printf("[gocam]     Format:      %s\n", formatName)
	}
	camLog.Printf("[gocam]     Resolution:  %d x %d\n", w, h)
	camLog.Printf("[gocam]     Buffer:      %d*3 (%d bytes)\n", bufPixels, bufBytes)
	stride := int(strideY)
	if stride > 0 {
		camLog.Printf("[gocam]     Stride:      %d bytes\n", stride)
	}
	camLog.Println("[gocam]     Conversion:")
	if isNV12 != 0 {
		camLog.Println("[gocam]       Pre Format Conversion:  NO  (already uncompressed)")
		camLog.Println("[gocam]       Post Format Conversion: YES (to YCbCr444)")
	} else {
		camLog.Println("[gocam]       Pre Format Conversion:  UNKNOWN")
		camLog.Println("[gocam]       Post Format Conversion: NO (unsupported)")
	}
	camLog.Println("[gocam]       Resampling:             NO")
}

// StartStream starts camera capture via Media Foundation on Windows
// and returns a channel of frames encoded as packed YCbCr 4:4:4 (YUV444).
func StartStream(ctx context.Context) (<-chan Frame, error) {
	hr := C.StartCapture()
	if hr != 0 {
		return nil, fmt.Errorf("gocam: cannot start capture, hr=0x%x", uint32(hr))
	}

	frames := make(chan Frame, 1)

	go func() {
		defer close(frames)
		defer C.StopCapture()

		const dropThreshold = 30
		misses := 0

		getFrameSize := func() (int, int, bool) {
			var cw, ch C.int
			if C.GetFrameSize(&cw, &ch) != 0 {
				return 0, 0, false
			}
			w := int(cw)
			h := int(ch)
			if w <= 0 || h <= 0 {
				return 0, 0, false
			}
			return w, h, true
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

		sendFrame := func(frame Frame) {
			select {
			case frames <- frame:
			default:
				<-frames
				frames <- frame
			}
		}

		sendBlack := func() bool {
			w, h, ok := getFrameSize()
			if !ok {
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

		var logged bool

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var cbuf *C.uchar
			var cw, ch C.int
			var csize C.int

			if C.GetFrame(&cbuf, &cw, &ch, &csize) != 0 || cbuf == nil {
				handleDrop(33*time.Millisecond, 10*time.Millisecond)
				continue
			}

			w := int(cw)
			h := int(ch)
			size := int(csize)
			if w <= 0 || h <= 0 || size <= 0 {
				handleDrop(33*time.Millisecond, 5*time.Millisecond)
				continue
			}

			data := C.GoBytes(unsafe.Pointer(cbuf), C.int(size))
			if len(data) == 0 {
				handleDrop(33*time.Millisecond, 5*time.Millisecond)
				continue
			}

			frame := Frame{
				Data:   data,
				Width:  w,
				Height: h,
			}

			misses = 0
			if !logged {
				logCameraConfig()
				logged = true
			}
			sendFrame(frame)
		}
	}()

	return frames, nil
}
