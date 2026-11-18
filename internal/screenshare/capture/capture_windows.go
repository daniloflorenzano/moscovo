//go:build windows

package capture

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	d3d11    = syscall.NewLazyDLL("d3d11.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	procEnumDisplayMonitors   = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW       = user32.NewProc("GetMonitorInfoW")
	procEnumWindows           = user32.NewProc("EnumWindows")
	procIsWindow              = user32.NewProc("IsWindow")
	procIsWindowVisible       = user32.NewProc("IsWindowVisible")
	procGetWindowTextW        = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW  = user32.NewProc("GetWindowTextLengthW")
	procGetWindowRect         = user32.NewProc("GetWindowRect")
	procGetDC                 = user32.NewProc("GetDC")
	procGetWindowDC           = user32.NewProc("GetWindowDC")
	procReleaseDC             = user32.NewProc("ReleaseDC")
	procPrintWindow           = user32.NewProc("PrintWindow")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")

	procDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
	procD3D11CreateDevice     = d3d11.NewProc("D3D11CreateDevice")
	procCoInitializeEx        = ole32.NewProc("CoInitializeEx")
	procCoUninitialize        = ole32.NewProc("CoUninitialize")
	procCoCreateInstance      = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree         = ole32.NewProc("CoTaskMemFree")
)

const (
	dwmwaCloaked        = 14
	monitorInfoPrimary  = 1
	srcCopy             = 0x00CC0020
	captureBlt          = 0x40000000
	pwRenderFullContent = 0x00000002
	dibRGBColors        = 0
	biRGB               = 0
	smCXScreen          = 0
	smCYScreen          = 1

	d3dDriverTypeHardware = 1
	d3d11SDKVersion       = 7
	d3d11UsageStaging     = 3
	d3d11CPUAccessRead    = 0x20000
	d3d11MapRead          = 1
	dxgiErrorWaitTimeout  = 0x887A0027

	clsctxAll                 = 23
	coinitMultithreaded       = 0x0
	eRender                   = 0
	eConsole                  = 0
	audclntShareModeShared    = 0
	audclntStreamFlagsLoopback = 0x00020000
	audclntBufferFlagsSilent  = 0x2
	waveFormatPCM             = 1
	waveFormatIEEEFloat       = 3
	waveFormatExtensible      = 0xFFFE
)

type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIDXGIDevice          = comGUID{0x54ec77fa, 0x1377, 0x44e6, [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidIDXGIOutput1         = comGUID{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture2D      = comGUID{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
	clsidMMDeviceEnumerator = comGUID{0xbcde0395, 0xe52f, 0x467c, [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iidIMMDeviceEnumerator  = comGUID{0xa95664d2, 0x9614, 0x4f35, [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	iidIAudioClient         = comGUID{0x1cb9ad4c, 0xdbfa, 0x4c32, [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	iidIAudioCaptureClient  = comGUID{0xc8adbd64, 0xe71e, 0x48a0, [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}
	ksSubtypeIEEEFloat      = comGUID{0x00000003, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

type monitorInfoExW struct {
	CbSize    uint32
	RcMonitor winRect
	RcWork    winRect
	DwFlags   uint32
	SzDevice  [32]uint16
}

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

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type winMonitor struct {
	id      string
	index   int
	title   string
	x, y    int
	w, h    int
	primary bool
}

type winWindow struct {
	hwnd  uintptr
	title string
	w, h  int
}

// IsWaylandSession always returns false on Windows.
func IsWaylandSession() bool {
	return false
}

// GetShareableSources enumerates Windows monitors and visible application windows with JPEG thumbnails.
func GetShareableSources(ctx context.Context) ([]domain.ShareableSource, error) {
	_ = ctx
	pool := encoder.NewFramePool()
	monitors := enumerateWinMonitors()
	sources := make([]domain.ShareableSource, 0, len(monitors)+12)

	for _, m := range monitors {
		var thumb string
		if img := captureRectGDI(m.x, m.y, m.w, m.h, pool); img != nil {
			thumb, _ = ThumbnailJPEGDataURL(img, 320, 180, pool)
			pool.PutRGBA(img)
		}
		if thumb == "" {
			thumb = GeneratePlaceholderThumbnail(m.title, domain.SourceTypeScreen, m.w, m.h)
		}
		sources = append(sources, domain.ShareableSource{
			ID:        m.id,
			Title:     m.title,
			Type:      domain.SourceTypeScreen,
			Thumbnail: thumb,
			Width:     m.w,
			Height:    m.h,
			IsPrimary: m.primary,
		})
	}

	windows := enumerateWinWindows()
	for _, w := range windows {
		var thumb string
		if img := captureWindowGDI(w.hwnd, pool); img != nil {
			thumb, _ = ThumbnailJPEGDataURL(img, 320, 180, pool)
			pool.PutRGBA(img)
		}
		if thumb == "" {
			thumb = GeneratePlaceholderThumbnail(w.title, domain.SourceTypeWindow, w.w, w.h)
		}
		sources = append(sources, domain.ShareableSource{
			ID:        fmt.Sprintf("window:0x%x", w.hwnd),
			Title:     w.title,
			Type:      domain.SourceTypeWindow,
			Thumbnail: thumb,
			Width:     w.w,
			Height:    w.h,
		})
	}

	return sources, nil
}

// NewPlatformCapturer creates a Windows DXGI/GDI screen/window capturer with optional WASAPI loopback audio.
func NewPlatformCapturer(cfg CaptureConfig, pool *encoder.FramePool) (Capturer, error) {
	if pool == nil {
		pool = encoder.NewFramePool()
	}
	return newWindowsCapturer(cfg, pool), nil
}

func enumerateWinMonitors() []winMonitor {
	var monitors []winMonitor
	cb := syscall.NewCallback(func(hMon uintptr, hdc uintptr, rc *winRect, data uintptr) uintptr {
		var mi monitorInfoExW
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi)))
		if r == 0 {
			return 1
		}
		w := int(mi.RcMonitor.Right - mi.RcMonitor.Left)
		h := int(mi.RcMonitor.Bottom - mi.RcMonitor.Top)
		if w <= 0 || h <= 0 {
			return 1
		}
		idx := len(monitors)
		devName := syscall.UTF16ToString(mi.SzDevice[:])
		if devName == "" {
			devName = fmt.Sprintf("Display %d", idx+1)
		}
		monitors = append(monitors, winMonitor{
			id:      fmt.Sprintf("screen:%d", idx),
			index:   idx,
			title:   fmt.Sprintf("Display %d (%dx%d)", idx+1, w, h),
			x:       int(mi.RcMonitor.Left),
			y:       int(mi.RcMonitor.Top),
			w:       w,
			h:       h,
			primary: mi.DwFlags&monitorInfoPrimary != 0,
		})
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)

	if len(monitors) == 0 {
		w, _, _ := procGetSystemMetrics.Call(smCXScreen)
		h, _, _ := procGetSystemMetrics.Call(smCYScreen)
		if w == 0 || h == 0 {
			w, h = 1920, 1080
		}
		monitors = append(monitors, winMonitor{
			id:      "screen:0",
			index:   0,
			title:   fmt.Sprintf("Primary Display (%dx%d)", w, h),
			w:       int(w),
			h:       int(h),
			primary: true,
		})
	}
	return monitors
}

func enumerateWinWindows() []winWindow {
	var windows []winWindow
	cb := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		var cloaked uint32
		r, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), 4)
		if r == 0 && cloaked != 0 {
			return 1
		}
		tLen, _, _ := procGetWindowTextLengthW.Call(hwnd)
		if tLen <= 0 || tLen > 512 {
			return 1
		}
		buf := make([]uint16, tLen+1)
		procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		title := strings.TrimSpace(syscall.UTF16ToString(buf))
		if title == "" {
			return 1
		}
		var rc winRect
		ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		if ok == 0 {
			return 1
		}
		w := int(rc.Right - rc.Left)
		h := int(rc.Bottom - rc.Top)
		if w < 64 || h < 64 {
			return 1
		}
		windows = append(windows, winWindow{
			hwnd:  hwnd,
			title: title,
			w:     w,
			h:     h,
		})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return windows
}

func captureRectGDI(x, y, w, h int, pool *encoder.FramePool) *image.RGBA {
	if w <= 0 || h <= 0 {
		return nil
	}
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return nil
	}
	defer procReleaseDC.Call(0, hdcScreen)

	return captureFromDC(hdcScreen, x, y, w, h, false, 0, pool)
}

func captureWindowGDI(hwnd uintptr, pool *encoder.FramePool) *image.RGBA {
	if ok, _, _ := procIsWindow.Call(hwnd); ok == 0 {
		return nil
	}
	var rc winRect
	if ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); ok == 0 {
		return nil
	}
	w := int(rc.Right - rc.Left)
	h := int(rc.Bottom - rc.Top)
	if w < 2 || h < 2 {
		return nil
	}
	hdcWin, _, _ := procGetWindowDC.Call(hwnd)
	if hdcWin == 0 {
		return nil
	}
	defer procReleaseDC.Call(hwnd, hdcWin)

	return captureFromDC(hdcWin, 0, 0, w, h, true, hwnd, pool)
}

func captureFromDC(hdcSrc uintptr, x, y, w, h int, usePrintWindow bool, hwnd uintptr, pool *encoder.FramePool) *image.RGBA {
	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcSrc)
	if hdcMem == 0 {
		return nil
	}
	defer procDeleteDC.Call(hdcMem)

	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdcSrc, uintptr(w), uintptr(h))
	if hBmp == 0 {
		return nil
	}
	defer procDeleteObject.Call(hBmp)

	oldObj, _, _ := procSelectObject.Call(hdcMem, hBmp)
	defer procSelectObject.Call(hdcMem, oldObj)

	if usePrintWindow && hwnd != 0 {
		ok, _, _ := procPrintWindow.Call(hwnd, hdcMem, pwRenderFullContent)
		if ok == 0 {
			ok, _, _ = procPrintWindow.Call(hwnd, hdcMem, 0)
		}
		if ok == 0 {
			procBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcSrc, 0, 0, srcCopy)
		}
	} else {
		ok, _, _ := procBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcSrc, uintptr(x), uintptr(y), srcCopy|captureBlt)
		if ok == 0 {
			return nil
		}
	}

	var bmi bitmapInfo
	bmi.Header.Size = uint32(unsafe.Sizeof(bmi.Header))
	bmi.Header.Width = int32(w)
	bmi.Header.Height = -int32(h) // top-down DIB
	bmi.Header.Planes = 1
	bmi.Header.BitCount = 32
	bmi.Header.Compression = biRGB

	img := pool.GetRGBA(w, h)
	lines, _, _ := procGetDIBits.Call(
		hdcMem,
		hBmp,
		0,
		uintptr(h),
		uintptr(unsafe.Pointer(&img.Pix[0])),
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
	)
	if lines == 0 {
		pool.PutRGBA(img)
		return nil
	}

	// Convert BGRA from Windows DIB to RGBA in place.
	pix := img.Pix[:w*h*4]
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+2], pix[i+3] = pix[i+2], pix[i], 255
	}
	return img
}

type windowsCapturer struct {
	cfg  CaptureConfig
	pool *encoder.FramePool

	framesCh chan FramePacket
	audioCh  chan []byte

	mu          sync.Mutex
	started     bool
	closed      bool
	runErr      error
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

func newWindowsCapturer(cfg CaptureConfig, pool *encoder.FramePool) *windowsCapturer {
	var audioCh chan []byte
	if cfg.IncludeSystemAudio {
		audioCh = make(chan []byte, 16)
	}
	return &windowsCapturer{
		cfg:      cfg,
		pool:     pool,
		framesCh: make(chan FramePacket, 2),
		audioCh:  audioCh,
	}
}

func (c *windowsCapturer) Frames() <-chan FramePacket { return c.framesCh }
func (c *windowsCapturer) AudioFrames() <-chan []byte  { return c.audioCh }
func (c *windowsCapturer) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runErr
}

func (c *windowsCapturer) setErr(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.runErr == nil {
		c.runErr = err
	}
	c.mu.Unlock()
}

func (c *windowsCapturer) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("windows capturer closed")
	}
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.started = true
	c.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	if c.cfg.IncludeSystemAudio {
		c.wg.Add(1)
		go c.wasapiLoopbackLoop(runCtx)
	}

	c.wg.Add(1)
	go c.videoLoop(runCtx)
	return nil
}

func (c *windowsCapturer) videoLoop(ctx context.Context) {
	defer c.wg.Done()
	defer c.closeChannels()

	if strings.HasPrefix(c.cfg.SourceID, "window:") {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(c.cfg.SourceID, "window:"), "0x"), 16, 64)
		if err != nil || parsed == 0 {
			c.setErr(fmt.Errorf("invalid window handle %q", c.cfg.SourceID))
			return
		}
		c.runWindowGDILoop(ctx, uintptr(parsed))
		return
	}

	monitors := enumerateWinMonitors()
	idx := 0
	if strings.HasPrefix(c.cfg.SourceID, "screen:") {
		if n, err := strconv.Atoi(strings.TrimPrefix(c.cfg.SourceID, "screen:")); err == nil && n >= 0 && n < len(monitors) {
			idx = n
		}
	}
	mon := monitors[idx]

	// Attempt DXGI Desktop Duplication first (NFR-01/NFR-02); fall back seamlessly to GDI BitBlt if unavailable.
	if err := c.runDXGIDuplicationLoop(ctx, mon); err != nil && ctx.Err() == nil {
		c.runMonitorGDILoop(ctx, mon)
	}
}

func (c *windowsCapturer) runWindowGDILoop(ctx context.Context, hwnd uintptr) {
	interval := time.Second / time.Duration(max(1, c.cfg.FPS))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ok, _, _ := procIsWindow.Call(hwnd); ok == 0 {
				c.setErr(ErrSourceClosed)
				return
			}
			img := captureWindowGDI(hwnd, c.pool)
			if img == nil {
				continue
			}
			c.emitFrame(ctx, img)
		}
	}
}

func (c *windowsCapturer) runMonitorGDILoop(ctx context.Context, mon winMonitor) {
	interval := time.Second / time.Duration(max(1, c.cfg.FPS))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			img := captureRectGDI(mon.x, mon.y, mon.w, mon.h, c.pool)
			if img == nil {
				continue
			}
			c.emitFrame(ctx, img)
		}
	}
}

func (c *windowsCapturer) emitFrame(ctx context.Context, img *image.RGBA) {
	pkt := FramePacket{
		Image:      img,
		CapturedAt: time.Now(),
		Release:    func() { c.pool.PutRGBA(img) },
	}
	select {
	case <-ctx.Done():
		pkt.Release()
	case c.framesCh <- pkt:
	default:
		pkt.Release()
	}
}

// --- DXGI Desktop Duplication COM Wrapper (zero CGO) ---

type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

type d3d11MappedSubresource struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

type dxgiOutDuplFrameInfo struct {
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerPosition           [12]byte
	TotalMetadataBufferSize   uint32
	PointerShapeBufferSize    uint32
}

type comObject struct {
	vtbl *[128]uintptr
}

func comCall(obj unsafe.Pointer, vtableIndex int, args ...uintptr) uintptr {
	iface := (*comObject)(obj)
	fnPtr := iface.vtbl[vtableIndex]
	callArgs := make([]uintptr, 0, len(args)+1)
	callArgs = append(callArgs, uintptr(obj))
	callArgs = append(callArgs, args...)
	r, _, _ := syscall.SyscallN(fnPtr, callArgs...)
	return r
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, 2)
	}
}

func (c *windowsCapturer) runDXGIDuplicationLoop(ctx context.Context, mon winMonitor) error {
	var d3dDev, d3dCtx unsafe.Pointer
	var featureLevel uintptr
	hr, _, _ := procD3D11CreateDevice.Call(
		0,
		d3dDriverTypeHardware,
		0, 0, 0, 0,
		d3d11SDKVersion,
		uintptr(unsafe.Pointer(&d3dDev)),
		uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&d3dCtx)),
	)
	if int32(hr) < 0 || d3dDev == nil || d3dCtx == nil {
		return fmt.Errorf("D3D11CreateDevice failed: 0x%x", uint32(hr))
	}
	defer comRelease(d3dCtx)
	defer comRelease(d3dDev)

	var dxgiDev unsafe.Pointer
	if r := comCall(d3dDev, 0, uintptr(unsafe.Pointer(&iidIDXGIDevice)), uintptr(unsafe.Pointer(&dxgiDev))); int32(r) < 0 || dxgiDev == nil {
		return fmt.Errorf("QueryInterface(IDXGIDevice) failed: 0x%x", uint32(r))
	}
	defer comRelease(dxgiDev)

	var adapter unsafe.Pointer
	if r := comCall(dxgiDev, 7, uintptr(unsafe.Pointer(&adapter))); int32(r) < 0 || adapter == nil {
		return fmt.Errorf("IDXGIDevice::GetAdapter failed: 0x%x", uint32(r))
	}
	defer comRelease(adapter)

	var output unsafe.Pointer
	if r := comCall(adapter, 7, uintptr(mon.index), uintptr(unsafe.Pointer(&output))); int32(r) < 0 || output == nil {
		return fmt.Errorf("IDXGIAdapter::EnumOutputs failed: 0x%x", uint32(r))
	}
	defer comRelease(output)

	var output1 unsafe.Pointer
	if r := comCall(output, 0, uintptr(unsafe.Pointer(&iidIDXGIOutput1)), uintptr(unsafe.Pointer(&output1))); int32(r) < 0 || output1 == nil {
		return fmt.Errorf("QueryInterface(IDXGIOutput1) failed: 0x%x", uint32(r))
	}
	defer comRelease(output1)

	var dupl unsafe.Pointer
	if r := comCall(output1, 22, uintptr(d3dDev), uintptr(unsafe.Pointer(&dupl))); int32(r) < 0 || dupl == nil {
		return fmt.Errorf("IDXGIOutput1::DuplicateOutput failed: 0x%x", uint32(r))
	}
	defer comRelease(dupl)

	stagingDesc := d3d11Texture2DDesc{
		Width:          uint32(mon.w),
		Height:         uint32(mon.h),
		MipLevels:      1,
		ArraySize:      1,
		Format:         87, // DXGI_FORMAT_B8G8R8A8_UNORM
		SampleCount:    1,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CPUAccessRead,
	}
	var stagingTex unsafe.Pointer
	if r := comCall(d3dDev, 5, uintptr(unsafe.Pointer(&stagingDesc)), 0, uintptr(unsafe.Pointer(&stagingTex))); int32(r) < 0 || stagingTex == nil {
		return fmt.Errorf("CreateTexture2D staging failed: 0x%x", uint32(r))
	}
	defer comRelease(stagingTex)

	interval := time.Second / time.Duration(max(1, c.cfg.FPS))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastImg *image.RGBA
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var frameInfo dxgiOutDuplFrameInfo
			var desktopRes unsafe.Pointer
			r := comCall(dupl, 8, 16, uintptr(unsafe.Pointer(&frameInfo)), uintptr(unsafe.Pointer(&desktopRes)))
			if uint32(r) == dxgiErrorWaitTimeout {
				// Re-emit most recent frame or wait for next desktop update.
				if lastImg != nil {
					dup := c.pool.GetRGBA(lastImg.Rect.Dx(), lastImg.Rect.Dy())
					copy(dup.Pix, lastImg.Pix)
					c.emitFrame(ctx, dup)
				}
				continue
			}
			if int32(r) < 0 {
				return fmt.Errorf("AcquireNextFrame failed: 0x%x", uint32(r))
			}

			var srcTex unsafe.Pointer
			if desktopRes != nil {
				q := comCall(desktopRes, 0, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&srcTex)))
				if int32(q) >= 0 && srcTex != nil {
					comCall(d3dCtx, 48, uintptr(stagingTex), uintptr(srcTex)) // ID3D11DeviceContext::CopyResource
					comRelease(srcTex)
				}
				comRelease(desktopRes)
			}
			comCall(dupl, 14) // IDXGIOutputDuplication::ReleaseFrame

			var mapped d3d11MappedSubresource
			mr := comCall(d3dCtx, 14, uintptr(stagingTex), 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&mapped)))
			if int32(mr) >= 0 && mapped.Data != nil {
				img := c.pool.GetRGBA(mon.w, mon.h)
				rowBytes := mon.w * 4
				pitch := int(mapped.RowPitch)
				mappedBuf := unsafe.Slice((*byte)(mapped.Data), pitch*mon.h)
				for y := 0; y < mon.h; y++ {
					srcRow := mappedBuf[y*pitch : y*pitch+rowBytes]
					dstRow := img.Pix[y*img.Stride : y*img.Stride+rowBytes]
					for x := 0; x < rowBytes; x += 4 {
						dstRow[x] = srcRow[x+2]
						dstRow[x+1] = srcRow[x+1]
						dstRow[x+2] = srcRow[x]
						dstRow[x+3] = 255
					}
				}
				comCall(d3dCtx, 15, uintptr(stagingTex), 0) // ID3D11DeviceContext::Unmap

				if lastImg == nil || lastImg.Rect.Dx() != mon.w || lastImg.Rect.Dy() != mon.h {
					lastImg = image.NewRGBA(image.Rect(0, 0, mon.w, mon.h))
				}
				copy(lastImg.Pix, img.Pix)
				c.emitFrame(ctx, img)
			}
		}
	}
}

// --- WASAPI Desktop Audio Loopback Capture (FR-06, zero CGO) ---

type wasapiWaveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type wasapiWaveFormatExtensible struct {
	Format      wasapiWaveFormatEx
	ValidBits   uint16
	ChannelMask uint32
	SubFormat   comGUID
}

func (c *windowsCapturer) wasapiLoopbackLoop(ctx context.Context) {
	defer c.wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if int32(hr) >= 0 {
		defer procCoUninitialize.Call()
	}

	var enumPtr unsafe.Pointer
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumPtr)),
	)
	if int32(r) < 0 || enumPtr == nil {
		return
	}
	defer comRelease(enumPtr)

	var devPtr unsafe.Pointer
	if r := comCall(enumPtr, 4, eRender, eConsole, uintptr(unsafe.Pointer(&devPtr))); int32(r) < 0 || devPtr == nil {
		return
	}
	defer comRelease(devPtr)

	var audioClient unsafe.Pointer
	if r := comCall(devPtr, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&audioClient))); int32(r) < 0 || audioClient == nil {
		return
	}
	defer comRelease(audioClient)

	var mixFmtPtr unsafe.Pointer
	if r := comCall(audioClient, 8, uintptr(unsafe.Pointer(&mixFmtPtr))); int32(r) < 0 || mixFmtPtr == nil {
		return
	}
	defer procCoTaskMemFree.Call(uintptr(mixFmtPtr))

	wfx := (*wasapiWaveFormatEx)(mixFmtPtr)
	isFloat := wfx.FormatTag == waveFormatIEEEFloat
	if wfx.FormatTag == waveFormatExtensible && wfx.Size >= 22 {
		ext := (*wasapiWaveFormatExtensible)(mixFmtPtr)
		isFloat = ext.SubFormat == ksSubtypeIEEEFloat
	}
	srcRate := int(wfx.SamplesPerSec)
	srcCh := int(wfx.Channels)
	srcBits := int(wfx.BitsPerSample)
	blockAlign := int(wfx.BlockAlign)
	if srcRate <= 0 || srcCh <= 0 || blockAlign <= 0 {
		return
	}

	// 100ms buffer in 100ns units.
	const bufferDuration100ns = uintptr(1_000_000)
	if r := comCall(audioClient, 3, audclntShareModeShared, audclntStreamFlagsLoopback, bufferDuration100ns, 0, uintptr(mixFmtPtr), 0); int32(r) < 0 {
		return
	}

	var capClient unsafe.Pointer
	if r := comCall(audioClient, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capClient))); int32(r) < 0 || capClient == nil {
		return
	}
	defer comRelease(capClient)

	if r := comCall(audioClient, 10); int32(r) < 0 {
		return
	}
	defer comCall(audioClient, 11)

	var accum []byte
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				var packetFrames uint32
				if r := comCall(capClient, 5, uintptr(unsafe.Pointer(&packetFrames))); int32(r) < 0 || packetFrames == 0 {
					break
				}
				var dataPtr unsafe.Pointer
				var numFrames uint32
				var flags uint32
				r := comCall(capClient, 3, uintptr(unsafe.Pointer(&dataPtr)), uintptr(unsafe.Pointer(&numFrames)), uintptr(unsafe.Pointer(&flags)), 0, 0)
				if int32(r) < 0 {
					break
				}

				if numFrames > 0 {
					if flags&audclntBufferFlagsSilent != 0 || dataPtr == nil {
						silentFrames := (int(numFrames) * encoder.SystemAudioCaptureRate) / srcRate
						accum = append(accum, make([]byte, silentFrames*4)...)
					} else {
						raw := unsafe.Slice((*byte)(dataPtr), int(numFrames)*blockAlign)
						converted := convertWASAPITo48kStereoS16(raw, int(numFrames), srcRate, srcCh, srcBits, isFloat)
						accum = append(accum, converted...)
					}
				}
				comCall(capClient, 4, uintptr(numFrames)) // ReleaseBuffer

				for len(accum) >= encoder.SystemAudioFrameBytes48kStereo {
					chunk := make([]byte, encoder.SystemAudioFrameBytes48kStereo)
					copy(chunk, accum[:encoder.SystemAudioFrameBytes48kStereo])
					accum = accum[encoder.SystemAudioFrameBytes48kStereo:]
					select {
					case <-ctx.Done():
						return
					case c.audioCh <- chunk:
					default:
						select {
						case <-c.audioCh:
						default:
						}
						select {
						case c.audioCh <- chunk:
						default:
						}
					}
				}
			}
		}
	}
}

func convertWASAPITo48kStereoS16(raw []byte, numFrames, srcRate, srcCh, srcBits int, isFloat bool) []byte {
	outFrames := (numFrames * encoder.SystemAudioCaptureRate) / srcRate
	if outFrames <= 0 {
		return nil
	}
	out := make([]byte, outFrames*4)
	bytesPerSample := max(1, srcBits/8)
	frameStride := srcCh * bytesPerSample

	readSample := func(frameIdx, chIdx int) int16 {
		if chIdx >= srcCh {
			chIdx = 0
		}
		off := frameIdx*frameStride + chIdx*bytesPerSample
		if off+bytesPerSample > len(raw) {
			return 0
		}
		if isFloat && bytesPerSample == 4 {
			v := math.Float32frombits(binary.LittleEndian.Uint32(raw[off : off+4]))
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			return int16(v * 32767)
		}
		if bytesPerSample >= 2 {
			return int16(binary.LittleEndian.Uint16(raw[off : off+2]))
		}
		return 0
	}

	for i := 0; i < outFrames; i++ {
		srcIdx := (i * numFrames) / outFrames
		l := readSample(srcIdx, 0)
		r := l
		if srcCh > 1 {
			r = readSample(srcIdx, 1)
		}
		binary.LittleEndian.PutUint16(out[i*4:i*4+2], uint16(l))
		binary.LittleEndian.PutUint16(out[i*4+2:i*4+4], uint16(r))
	}
	return out
}

func (c *windowsCapturer) closeChannels() {
	c.closeOnce.Do(func() {
		close(c.framesCh)
		if c.audioCh != nil {
			close(c.audioCh)
		}
	})
}

func (c *windowsCapturer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.wg.Wait()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	return nil
}
