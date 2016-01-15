package vips

/*
#cgo pkg-config: vips
#include "vips.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"unsafe"
	"strconv"
)

const DEBUG = false

var (
	MARKER_JPEG = []byte{0xff, 0xd8}
	MARKER_PNG  = []byte{0x89, 0x50}
    MARKER_WEBP  = []byte{0x57, 0x45, 0x42, 0x50}
    MARKER_RIFF  = []byte{0x52, 0x49, 0x46, 0x46}
)

type ImageType int

const (
	UNKNOWN ImageType = iota
	JPEG
	PNG
	WEBP
)

type Interpolator int

const (
	BICUBIC Interpolator = iota
	BILINEAR
	NOHALO
)

type Extend int

const (
	EXTEND_BLACK Extend = C.VIPS_EXTEND_BLACK
	EXTEND_WHITE Extend = C.VIPS_EXTEND_WHITE
)

var interpolations = map[Interpolator]string{
	BICUBIC:  "bicubic",
	BILINEAR: "bilinear",
	NOHALO:   "nohalo",
}

type Angle int

const (
	D0   Angle = 0
	D90  Angle = 90
	D180 Angle = 180
	D270 Angle = 270
)

type Direction int

const (
	HORIZONTAL Direction = C.VIPS_DIRECTION_HORIZONTAL
	VERTICAL   Direction = C.VIPS_DIRECTION_VERTICAL
)

func (i Interpolator) String() string { return interpolations[i] }

type Options struct {
	Height       int
	Width        int
	Crop         bool
	Enlarge      bool
	Extend       Extend
	Embed        bool
	Interpolator Interpolator
	Gravity      Gravity
	Quality      int
	LeftPos      float32
	TopPos       float32
	Savetype     ImageType
	NoAutoRotate bool
	Rotate Angle
	Flip bool
	Flop bool
}

func init() {
	Initialize()
}

var initialized bool

func Initialize() {
	if initialized {
		return
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := C.vips_initialize(); err != 0 {
		C.vips_shutdown()
		panic("unable to start vips!")
	}

	C.vips_concurrency_set(1)
	C.vips_cache_set_max_mem(100 * 1048576) // 100Mb
	C.vips_cache_set_max(500)

	initialized = true
}

func Shutdown() {
	if !initialized {
		return
	}

	C.vips_shutdown()

	initialized = false
}

func Debug() {
	C.im__print_all()
}

func Resize(buf []byte, o Options) ([]byte, error) {
	debug("%#+v", o)

	// detect (if possible) the file type
	typ := UNKNOWN
	switch {
	case bytes.Equal(buf[:2], MARKER_JPEG):
		typ = JPEG
	case bytes.Equal(buf[:2], MARKER_PNG):
		typ = PNG
    case bytes.Equal(buf[:4], MARKER_RIFF) && bytes.Equal(buf[8:12], MARKER_WEBP):
        typ = WEBP
	default:
		return nil, errors.New("unknown image format")
	}

	// create an image instance
	var image, tmpImage *C.struct__VipsImage

	// feed it
	switch typ {
	case JPEG:
		C.vips_jpegload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
	case PNG:
		C.vips_pngload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
    case WEBP:
        C.vips_webpload_buffer_custom(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
	}

	// cleanup
	defer func() {
		C.vips_thread_shutdown()
		C.vips_error_clear()
	}()

	// defaults
	if o.Quality == 0 {
		o.Quality = 100
	}

	// get WxH
	inWidth := int(image.Xsize)
	inHeight := int(image.Ysize)

	// prepare for factor
	factor := 0.0

	// image calculations
	switch {
	// Fixed width and height
	case o.Width > 0 && o.Height > 0:
		xf := float64(inWidth) / float64(o.Width)
		yf := float64(inHeight) / float64(o.Height)
		if o.Crop {
			factor = math.Min(xf, yf)
		} else {
			factor = math.Max(xf, yf)
		}
	// Fixed width, auto height
	case o.Width > 0:
		factor = float64(inWidth) / float64(o.Width)
		o.Height = int(math.Floor(float64(inHeight) / factor))
	// Fixed height, auto width
	case o.Height > 0:
		factor = float64(inHeight) / float64(o.Height)
		o.Width = int(math.Floor(float64(inWidth) / factor))
	// Identity transform
	default:
		factor = 1
		o.Width = inWidth
		o.Height = inHeight
	}

	debug("transform from %dx%d to %dx%d", inWidth, inHeight, o.Width, o.Height)

	// shrink
	shrink := int(math.Floor(factor))
	if shrink < 1 {
		shrink = 1
	}

	// residual
	residual := float64(shrink) / factor

	// Do not enlarge the output if the input width *or* height are already less than the required dimensions
	if !o.Enlarge {
		if inWidth < o.Width && inHeight < o.Height {
			factor = 1
			shrink = 1
			residual = 0
			o.Width = inWidth
			o.Height = inHeight
		}
	}

	debug("factor: %v, shrink: %v, residual: %v", factor, shrink, residual)

	// Try to use libjpeg shrink-on-load
	shrinkOnLoad := 1
	if typ == JPEG && shrink >= 2 {
		switch {
		case shrink >= 8:
			factor = factor / 8
			shrinkOnLoad = 8
		case shrink >= 4:
			factor = factor / 4
			shrinkOnLoad = 4
		case shrink >= 2:
			factor = factor / 2
			shrinkOnLoad = 2
		}
	}

	if shrinkOnLoad > 1 {
		debug("shrink on load %d", shrinkOnLoad)
		// Recalculate integral shrink and double residual
		factor = math.Max(factor, 1.0)
		shrink = int(math.Floor(factor))
		residual = float64(shrink) / factor
		// Reload input using shrink-on-load
		err := C.vips_jpegload_buffer_shrink(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &tmpImage, C.int(shrinkOnLoad))
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}
	}

	if shrink > 1 {
		debug("shrink %d", shrink)
		// Use vips_shrink with the integral reduction
		err := C.vips_shrink_0(image, &tmpImage, C.double(float64(shrink)), C.double(float64(shrink)))
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}

		// Recalculate residual float based on dimensions of required vs shrunk images
		shrunkWidth := int(image.Xsize)
		shrunkHeight := int(image.Ysize)

		residualx := float64(o.Width) / float64(shrunkWidth)
		residualy := float64(o.Height) / float64(shrunkHeight)
		if o.Crop {
			residual = math.Max(residualx, residualy)
		} else {
			residual = math.Min(residualx, residualy)
		}
	}

	// Use vips_affine with the remaining float part
	debug("residual: %v", residual)
	if residual != 0 {
		debug("residual %.2f", residual)
		// Create interpolator - "bilinear" (default), "bicubic" or "nohalo"
		is := C.CString(o.Interpolator.String())
		interpolator := C.vips_interpolate_new(is)

		// Perform affine transformation
		err := C.vips_affine_interpolator(image, &tmpImage, C.double(residual), 0, 0, C.double(residual), interpolator)
		C.g_object_unref(C.gpointer(image))

		image = tmpImage

		C.free(unsafe.Pointer(is))
		C.g_object_unref(C.gpointer(interpolator))

		if err != 0 {
			return nil, resizeError()
		}
	}

	// Crop/embed
	affinedWidth := int(image.Xsize)
	affinedHeight := int(image.Ysize)

	if affinedWidth != o.Width || affinedHeight != o.Height {
		if o.Crop {
			// Crop
			debug("cropping")
			left, top := sharpCalcCrop(affinedWidth, affinedHeight, o.Width, o.Height, o.LeftPos, o.TopPos, o.Gravity)
			o.Width = int(math.Min(float64(affinedWidth), float64(o.Width)))
			o.Height = int(math.Min(float64(affinedHeight), float64(o.Height)))
			err := C.vips_extract_area_0(image, &tmpImage, C.int(left), C.int(top), C.int(o.Width), C.int(o.Height))
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
			if err != 0 {
				return nil, resizeError()
			}
		} else if o.Embed {
			debug("embedding with extend %d", o.Extend)
			left := (o.Width - affinedWidth) / 2
			top := (o.Height - affinedHeight) / 2
			err := C.vips_embed_extend(image, &tmpImage, C.int(left), C.int(top), C.int(o.Width), C.int(o.Height), C.int(o.Extend))
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
			if err != 0 {
				return nil, resizeError()
			}
		}
	} else {
		debug("canvased same as affined")
	}

	// Always convert to sRGB colour space
	C.vips_colourspace_0(image, &tmpImage, C.VIPS_INTERPRETATION_sRGB)
	C.g_object_unref(C.gpointer(image))
	image = tmpImage

	// Finally save
	length := C.size_t(0)
	var ptr unsafe.Pointer

	switch o.Savetype {
		case WEBP:
			C.vips_webpsave_custom(image, &ptr, &length, C.int(o.Quality))
		case PNG:
			C.vips_pngsave_custom(image, &ptr, &length, 1, C.int(o.Quality), 0)
		default:
			C.vips_jpegsave_custom(image, &ptr, &length, 1, C.int(o.Quality), 0)
	}

	C.g_object_unref(C.gpointer(image))

	// get back the buffer
	buf = C.GoBytes(ptr, C.int(length))
	C.g_free(C.gpointer(ptr))

	return buf, nil
}

func resizeError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	return errors.New(s)
}

type Gravity int

const (
	CENTRE Gravity = iota
	NORTH
	EAST
	SOUTH
	WEST
	CUSTOM
)

func sharpCalcCrop(inWidth, inHeight, outWidth, outHeight int, customLeftPos, customTopPos float32, gravity Gravity) (int, int) {
	left, top := 0, 0
	switch gravity {
	case NORTH:
		left = (inWidth - outWidth + 1) / 2
	case EAST:
		left = inWidth - outWidth
		top = (inHeight - outHeight + 1) / 2
	case SOUTH:
		left = (inWidth - outWidth + 1) / 2
		top = inHeight - outHeight
	case WEST:
		top = (inHeight - outHeight + 1) / 2
	case CUSTOM:
		customLeft := float32(inWidth) * customLeftPos
		customTop := float32(inHeight) * customTopPos
		if customLeft + float32(outWidth) > float32(inWidth) {
			left = inWidth - outWidth
		} else {
			left = int(customLeft)
		}

		if customTop + float32(outHeight) > float32(inHeight) {
			top = inHeight - outHeight
		} else {
			top = int(customTop)
		}
	default:
		left = (inWidth - outWidth + 1) / 2
		top = (inHeight - outHeight + 1) / 2
	}
	return left, top
}

func debug(format string, args ...interface{}) {
	if !DEBUG {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func catchVipsError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	C.vips_thread_shutdown()
	return errors.New(s)
}

func vipsExifOrientation(image *C.struct__VipsImage) int {
	orientation := int(C.vips_exif_orientation(image))
	return orientation
}

func vipsRotate(image *C.struct__VipsImage, angle Angle) (*C.struct__VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_rotate(image, &out, C.int(angle))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsFlip(image *C.struct__VipsImage, direction Direction) (*C.struct__VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_flip_bridge(image, &out, C.int(direction))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func getAngle(angle Angle) Angle {
	divisor := angle % 90
	if divisor != 0 {
		angle = angle - divisor
	}

	debug("Angle:" + strconv.Itoa(int(angle)))
	return Angle(math.Min(float64(angle), 270))
}

func calculateRotationAndFlip(image *C.struct__VipsImage, angle Angle) (Angle, bool) {
	rotate := D0
	flip := false

	if angle > 0 {
		return rotate, flip
	}

	switch vipsExifOrientation(image) {
	case 6:
		rotate = D90
		break
	case 3:
		rotate = D180
		break
	case 8:
		rotate = D270
		break
	case 2:
		flip = true
		break // flip 1
	case 7:
		flip = true
		rotate = D90
		break // flip 6
	case 4:
		flip = true
		rotate = D180
		break // flip 3
	case 5:
		flip = true
		rotate = D270
		break // flip 8
	}

	return rotate, flip
}

func rotateAndFlipImage(image *C.struct__VipsImage, o Options) (*C.struct__VipsImage, error) {
	var err error
	var direction Direction = -1

	if o.NoAutoRotate == false {
		rotation, flip := calculateRotationAndFlip(image, o.Rotate)
		if flip {
			o.Flip = flip
		}
		if rotation > D0 && o.Rotate == 0 {
			o.Rotate = rotation
		}
	}

	if o.Rotate > 0 {
		image, err = vipsRotate(image, getAngle(o.Rotate))
	}

	if o.Flip {
		direction = HORIZONTAL
	} else if o.Flop {
		direction = VERTICAL
	}

	if direction != -1 {
		image, err = vipsFlip(image, direction)
	}

	return image, err
}

func AutoRotate(file string, o Options) ([]byte, error) {
	debug("%#+v", o)

   	// detect (if possible) the file type
   	/*typ := UNKNOWN
   	switch {
   	case bytes.Equal(buf[:2], MARKER_JPEG):
   		typ = JPEG
   	case bytes.Equal(buf[:2], MARKER_PNG):
   		typ = PNG
   	default:
   		return nil, errors.New("unknown image format")
   	}

   	// create an image instance
   	var image, tmpImage *C.struct__VipsImage

	// feed it
   	switch typ {
   	case JPEG:
   		C.vips_jpegload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
   	case PNG:
   		C.vips_pngload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
   	}*/
	// create an image instance
   	var image, tmpImage *C.struct__VipsImage

	image = C.vips_load_from_file(C.CString(file))

	// cleanup
   	defer func() {
   		C.vips_thread_shutdown()
   		C.vips_error_clear()
   	}()

	if image == nil {
		return nil, catchVipsError()
	}

	rotate,flip :=calculateRotationAndFlip(image, 0)

	if(rotate == 0 && !flip) {
		return nil, nil //Remain unchanged
	}

	ret := C.vips_autorotate(image, &tmpImage)

	if ret != 0 {
		return nil, catchVipsError()
	}

	if o.Quality == 0 {
		o.Quality = 100
	}

	length := C.size_t(0)
	var ptr unsafe.Pointer

	switch o.Savetype {
		case WEBP:
			C.vips_webpsave_custom(tmpImage, &ptr, &length, C.int(o.Quality))
		case PNG:
			C.vips_pngsave_custom(tmpImage, &ptr, &length, 1, C.int(o.Quality), 0)
		default:
			C.vips_jpegsave_custom(tmpImage, &ptr, &length, 1, C.int(o.Quality), 0)
	}

	C.g_object_unref(C.gpointer(tmpImage))

	// get back the buffer
	var buf []byte
	buf = C.GoBytes(ptr, C.int(length))
	C.g_free(C.gpointer(ptr))
	return buf, nil
}
