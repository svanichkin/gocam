//go:build linux && (amd64 || arm64 || riscv64 || ppc64le || loong64)

package gocam

import "unsafe"

// Compile-time checks that the ioctl structs match the kernel's layout on
// 64-bit Linux (sizes from linux/videodev2.h, v4l2-subdev.h and media.h):
// the size is part of each ioctl number, so a mismatch would make every
// call fail with ENOTTY.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2Capability{})-104]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2Format{})-208]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2FmtDesc{})-64]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2RequestBuffers{})-20]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2Buffer{})-88]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2Plane{})-64]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2PixFormatMPlane{})-192]
	_ = [1]struct{}{}[unsafe.Sizeof(mediaEntityDesc{})-256]
	_ = [1]struct{}{}[unsafe.Sizeof(mediaPadDesc{})-20]
	_ = [1]struct{}{}[unsafe.Sizeof(mediaLinkDesc{})-52]
	_ = [1]struct{}{}[unsafe.Sizeof(mediaLinksEnum{})-40]
	_ = [1]struct{}{}[unsafe.Sizeof(v4l2SubdevFormat{})-88]
	_ = [1]struct{}{}[unsafe.Offsetof(v4l2Buffer{}.M)-64]
	_ = [1]struct{}{}[unsafe.Offsetof(v4l2Plane{}.MemOffset)-8]
	_ = [1]struct{}{}[unsafe.Offsetof(mediaPadDesc{}.Flags)-8]
	_ = [1]struct{}{}[unsafe.Offsetof(mediaLinksEnum{}.Pads)-8]
)
