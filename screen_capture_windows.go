//go:build windows

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Design Ref: §2 (screenshot tool), §4 (screen_capture_windows.go).
//
// CGO-free full virtual-screen capture via Win32 GDI (GetDC/CreateCompatibleDC/
// CreateCompatibleBitmap/BitBlt/GetDIBits) → image.RGBA → PNG bytes.

var (
	modGdi32     = windows.NewLazySystemDLL("gdi32.dll")
	modUser32Cap = windows.NewLazySystemDLL("user32.dll")

	procGetDC                 = modUser32Cap.NewProc("GetDC")
	procReleaseDC             = modUser32Cap.NewProc("ReleaseDC")
	procGetSystemMetrics      = modUser32Cap.NewProc("GetSystemMetrics")
	procSetProcessDPIAware    = modUser32Cap.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwareCtx = modUser32Cap.NewProc("SetProcessDpiAwarenessContext")

	procCreateCompatibleDC     = modGdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = modGdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = modGdi32.NewProc("SelectObject")
	procBitBlt                 = modGdi32.NewProc("BitBlt")
	procGetDIBits              = modGdi32.NewProc("GetDIBits")
	procDeleteObject           = modGdi32.NewProc("DeleteObject")
	procDeleteDC               = modGdi32.NewProc("DeleteDC")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	srcCopy = 0x00CC0020

	biRGB        = 0
	dibRGBColors = 0

	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == -4 (as a handle value).
	dpiPerMonitorAwareV2 = ^uintptr(0) - 3 // -4 in two's complement
)

// bitmapInfoHeader mirrors the Win32 BITMAPINFOHEADER struct.
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// bitmapInfo with a single RGBQUAD entry (sufficient for 32bpp BI_RGB).
type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

var dpiOnce sync.Once

// ensureDPIAware makes the process DPI-aware once so that captured pixels and
// later click coordinates match the real physical screen. Prefers the modern
// per-monitor-v2 context, falling back to the legacy system-DPI call.
func ensureDPIAware() {
	dpiOnce.Do(func() {
		if procSetProcessDpiAwareCtx.Find() == nil {
			if r, _, _ := procSetProcessDpiAwareCtx.Call(dpiPerMonitorAwareV2); r != 0 {
				return
			}
		}
		procSetProcessDPIAware.Call()
	})
}

// captureScreen captures the full virtual screen and returns PNG bytes.
func captureScreen() ([]byte, error) {
	ensureDPIAware()

	x, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	y, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	w, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return captureRegion(int32(x), int32(y), int(int32(w)), int(int32(h)))
}

// captureWindow captures just the target window's screen rectangle and returns
// the PNG plus the window's top-left screen origin (so an in-image pixel (ix,iy)
// maps to screen (left+ix, top+iy)). Cropping to one window keeps the image small
// — often under the vision downscale cap — so it stays sharp and pixel-accurate.
func captureWindow(titleOrHwnd string) (png []byte, left, top, width, height int, err error) {
	ensureDPIAware()
	hwnd, ok := findTopWindow(titleOrHwnd)
	if !ok {
		return nil, 0, 0, 0, 0, fmt.Errorf("captureWindow: no window matching %q", titleOrHwnd)
	}
	// A window on another virtual desktop is DWM-cloaked: a BitBlt of its screen
	// rect would capture the CURRENT desktop's pixels, not the window. Switch to
	// its desktop first (this also records a return anchor via bringToFront).
	if isWindowOnAnotherDesktop(hwnd) {
		_ = bringToFront(hwnd)
		time.Sleep(300 * time.Millisecond) // let the desktop switch + repaint settle
	}
	// A minimized window's GetWindowRect is the iconic placeholder (off-screen,
	// ~-32000,-32000) with a small positive size, so the zero-size guard below
	// won't catch it and BitBlt would capture unrelated off-screen pixels. Reject
	// it explicitly (as listControls does) instead of returning nonsense.
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		return nil, 0, 0, 0, 0, fmt.Errorf("captureWindow: window %q is minimized (its rect is an off-screen placeholder); focus_window it first", titleOrHwnd)
	}
	rc := windowRect(hwnd)
	width = int(rc.Right - rc.Left)
	height = int(rc.Bottom - rc.Top)
	if width <= 0 || height <= 0 {
		return nil, 0, 0, 0, 0, fmt.Errorf("captureWindow: window %q has zero size", titleOrHwnd)
	}
	png, err = captureRegion(rc.Left, rc.Top, width, height)
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	return png, int(rc.Left), int(rc.Top), width, height, nil
}

// captureRegionAt captures a width×height rectangle and returns the PNG plus the
// rectangle's absolute screen origin (so an in-image pixel (ix,iy) maps to screen
// (origin+ix, origin+iy)). When window == "" the (x,y) are absolute screen
// coordinates; otherwise they are interpreted relative to that window's top-left
// (and, if the window is on another virtual desktop, we switch to it first — as
// capture_window does — so the pixels are the real window, not a stale desktop).
func captureRegionAt(window string, x, y, width, height int) (png []byte, absX, absY int, err error) {
	ensureDPIAware()
	if width <= 0 || height <= 0 {
		return nil, 0, 0, fmt.Errorf("capture_region: width/height must be positive (got %dx%d)", width, height)
	}
	absX, absY = x, y
	if strings.TrimSpace(window) != "" {
		hwnd, ok := findTopWindow(window)
		if !ok {
			return nil, 0, 0, fmt.Errorf("capture_region: no window matching %q", window)
		}
		if isWindowOnAnotherDesktop(hwnd) {
			_ = bringToFront(hwnd)
			time.Sleep(300 * time.Millisecond)
		}
		// Window-relative coords are meaningless against a minimized window's
		// off-screen iconic rect — reject rather than capture bogus pixels.
		if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
			return nil, 0, 0, fmt.Errorf("capture_region: window %q is minimized (window-relative coords would be bogus); focus_window it first", window)
		}
		rc := windowRect(hwnd)
		absX = int(rc.Left) + x
		absY = int(rc.Top) + y
	}
	png, err = captureRegion(int32(absX), int32(absY), width, height)
	if err != nil {
		return nil, 0, 0, err
	}
	return png, absX, absY, nil
}

// captureRegion BitBlts a rectangle of the screen (source top-left at srcX,srcY in
// screen coordinates) into a width×height bitmap and returns it as PNG bytes.
func captureRegion(srcX, srcY int32, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("captureRegion: invalid size %dx%d", width, height)
	}
	originX := srcX
	originY := srcY

	// Source DC for the whole screen (GetDC(NULL)).
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("captureScreen: GetDC failed")
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("captureScreen: CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memDC)

	bmp, _, _ := procCreateCompatibleBitmap.Call(screenDC, uintptr(width), uintptr(height))
	if bmp == 0 {
		return nil, fmt.Errorf("captureScreen: CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(bmp)

	old, _, _ := procSelectObject.Call(memDC, bmp)
	if old != 0 {
		defer procSelectObject.Call(memDC, old)
	}

	// Copy screen → memory bitmap, accounting for the virtual-screen origin.
	if r, _, _ := procBitBlt.Call(
		memDC, 0, 0, uintptr(width), uintptr(height),
		screenDC, uintptr(originX), uintptr(originY), srcCopy,
	); r == 0 {
		return nil, fmt.Errorf("captureScreen: BitBlt failed")
	}

	// Request a top-down 32bpp BI_RGB DIB (negative height → top-down rows).
	bi := bitmapInfo{
		Header: bitmapInfoHeader{
			Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			Width:       int32(width),
			Height:      -int32(height),
			Planes:      1,
			BitCount:    32,
			Compression: biRGB,
		},
	}

	buf := make([]byte, width*height*4)
	if r, _, _ := procGetDIBits.Call(
		memDC, bmp, 0, uintptr(height),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
	); r == 0 {
		return nil, fmt.Errorf("captureScreen: GetDIBits failed")
	}

	// GetDIBits gives BGRA for 32bpp BI_RGB; swap to RGBA and set alpha opaque.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i+3 < len(buf); i += 4 {
		b := buf[i]
		g := buf[i+1]
		r := buf[i+2]
		img.Pix[i] = r
		img.Pix[i+1] = g
		img.Pix[i+2] = b
		img.Pix[i+3] = 0xFF
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("captureScreen: png encode: %w", err)
	}
	return out.Bytes(), nil
}

// downscaleNearest returns a dw×dh nearest-neighbor-scaled copy of src. Shared by
// the screenshot downscale paths (captureScreenScaled / captureScreenCapped).
// Nearest-neighbor keeps it dependency-free and fast; Claude's vision reads the
// result fine for "what's on screen" purposes.
func downscaleNearest(src image.Image, dw, dh int) *image.RGBA {
	sb := src.Bounds()
	fx := float64(sb.Dx()) / float64(dw)
	fy := float64(sb.Dy()) / float64(dh)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		sy := sb.Min.Y + int(float64(dy)*fy)
		for dx := 0; dx < dw; dx++ {
			sx := sb.Min.X + int(float64(dx)*fx)
			dst.Set(dx, dy, src.At(sx, sy))
		}
	}
	return dst
}

// scaleReadingPNG downscales a PNG by an explicit factor in (0,1) for READING a
// window's content cheaply (fewer vision tokens), returning the new bytes and
// dimensions. scale<=0 or >=1 returns the input unchanged with its original
// dimensions. This is the caller-requested downscale, below the automatic
// visionLongEdgeCap that capture_window/region already apply; either way the 1:1
// image→screen click mapping is broken, so a scaled capture is for reading only
// and its caller must NOT offer image-pixel click coordinates.
func scaleReadingPNG(pngBytes []byte, scale float64) (out []byte, w, h int, err error) {
	src, derr := png.Decode(bytes.NewReader(pngBytes))
	if derr != nil {
		return nil, 0, 0, fmt.Errorf("scaleReadingPNG: decode: %w", derr)
	}
	sb := src.Bounds()
	if scale <= 0 || scale >= 1 {
		return pngBytes, sb.Dx(), sb.Dy(), nil
	}
	dw := int(float64(sb.Dx()) * scale)
	dh := int(float64(sb.Dy()) * scale)
	if dw < 1 || dh < 1 {
		return pngBytes, sb.Dx(), sb.Dy(), nil
	}
	enc, eerr := encodePNG(downscaleNearest(src, dw, dh))
	if eerr != nil {
		return nil, 0, 0, eerr
	}
	return enc, dw, dh, nil
}

// encodePNG PNG-encodes img to bytes.
func encodePNG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("png encode: %w", err)
	}
	return out.Bytes(), nil
}

// captureScreenScaled captures the screen and, when 0 < scale < 1, downscales
// the image with nearest-neighbor sampling before PNG-encoding. scale values
// outside (0,1) (or >=1) return the full-resolution capture.
func captureScreenScaled(scale float64) ([]byte, error) {
	if scale <= 0 || scale >= 1 {
		return captureScreen()
	}

	full, err := captureScreen()
	if err != nil {
		return nil, err
	}
	src, err := png.Decode(bytes.NewReader(full))
	if err != nil {
		return nil, fmt.Errorf("captureScreenScaled: decode: %w", err)
	}

	sb := src.Bounds()
	dw := int(float64(sb.Dx()) * scale)
	dh := int(float64(sb.Dy()) * scale)
	if dw < 1 || dh < 1 {
		return full, nil
	}
	return encodePNG(downscaleNearest(src, dw, dh))
}

// captureScreenCapped captures the screen and, when its longer edge exceeds
// maxLong px, downscales it so the longer edge equals maxLong (never upscales).
// This is the screenshot tool's default: Claude's vision already downscales
// anything over ~1568px on the long edge, so an uncapped 1080p/4K/multi-monitor
// grab all cost roughly the same ~1800 vision tokens — capping lower makes the
// per-screenshot token cost bounded and predictable. Clicking is unaffected:
// click coordinates come from win_controls/snapshot (OS pixels), and the full
// screenshot never promised a 1:1 image→screen pixel mapping (capture_window /
// capture_region stay full-resolution for that). maxLong<=0 or an already-small
// capture returns full resolution.
func captureScreenCapped(maxLong int) ([]byte, error) {
	full, err := captureScreen()
	if err != nil {
		return nil, err
	}
	if maxLong <= 0 {
		return full, nil
	}
	src, err := png.Decode(bytes.NewReader(full))
	if err != nil {
		return nil, fmt.Errorf("captureScreenCapped: decode: %w", err)
	}
	sb := src.Bounds()
	dw, dh, doScale := cappedDims(sb.Dx(), sb.Dy(), maxLong)
	if !doScale {
		return full, nil // already within budget — no quality loss
	}
	return encodePNG(downscaleNearest(src, dw, dh))
}

// capVisionLongEdge shrinks a PNG so its longer edge is at most visionLongEdgeCap,
// reporting whether it actually did. Unlike scaleReadingPNG (an explicit caller
// request) this is the automatic default for capture_window/capture_region: past
// the cap the vision layer downscales anyway, so the extra pixels buy nothing and
// only inflate the transcript. capped=false means the image already fit and is
// returned untouched, so the caller keeps its exact 1:1 click-mapping caption.
func capVisionLongEdge(pngBytes []byte) (out []byte, w, h int, capped bool, err error) {
	src, derr := png.Decode(bytes.NewReader(pngBytes))
	if derr != nil {
		return nil, 0, 0, false, fmt.Errorf("capVisionLongEdge: decode: %w", derr)
	}
	sb := src.Bounds()
	dw, dh, doScale := cappedDims(sb.Dx(), sb.Dy(), visionLongEdgeCap)
	if !doScale {
		return pngBytes, sb.Dx(), sb.Dy(), false, nil
	}
	enc, eerr := encodePNG(downscaleNearest(src, dw, dh))
	if eerr != nil {
		return nil, 0, 0, false, eerr
	}
	return enc, dw, dh, true, nil
}

// cappedDims computes the target dimensions for capping an w×h image so its
// longer edge is at most maxLong, preserving aspect ratio. doScale is false when
// the image already fits (or the inputs are degenerate), meaning "leave as-is".
// Pure (no image data) so the cap policy is unit-testable without a real screen.
func cappedDims(w, h, maxLong int) (dw, dh int, doScale bool) {
	if w <= 0 || h <= 0 || maxLong <= 0 {
		return w, h, false
	}
	longEdge := w
	if h > longEdge {
		longEdge = h
	}
	if longEdge <= maxLong {
		return w, h, false
	}
	scale := float64(maxLong) / float64(longEdge)
	dw = int(float64(w) * scale)
	dh = int(float64(h) * scale)
	if dw < 1 || dh < 1 {
		return w, h, false
	}
	return dw, dh, true
}
