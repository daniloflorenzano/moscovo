//go:build linux

package capture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

var xrandrLineRe = regexp.MustCompile(`^\s*\d+:\s+\+(\*?)(\S+)\s+(\d+)/\d+x(\d+)/\d+\+(\d+)\+(\d+)\s+(\S+)`)

type x11Monitor struct {
	id      string
	name    string
	x, y    int
	w, h    int
	primary bool
}

type x11Window struct {
	xid   uint32
	title string
	x, y  int
	w, h  int
}

// GetShareableSources discovers active monitors and visible windows (X11) or the ScreenCast portal (Wayland).
func GetShareableSources(ctx context.Context) ([]domain.ShareableSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pool := encoder.NewFramePool()

	if IsWaylandSession() {
		var thumb string
		if grimPath, err := exec.LookPath("grim"); err == nil {
			if img, gErr := captureGrimRGBA(ctx, grimPath, pool); gErr == nil {
				thumb, _ = ThumbnailJPEGDataURL(img, 320, 180, pool)
				pool.PutRGBA(img)
			}
		}
		if thumb == "" {
			thumb = GeneratePlaceholderThumbnail("Wayland ScreenCast Portal", domain.SourceTypeScreen, 1920, 1080)
		}
		return []domain.ShareableSource{
			{
				ID:        "wayland:portal",
				Title:     "Screen or Window (Wayland Portal)",
				Type:      domain.SourceTypeScreen,
				Thumbnail: thumb,
				Width:     1920,
				Height:    1080,
				IsPrimary: true,
			},
		}, nil
	}

	display := strings.TrimSpace(os.Getenv("DISPLAY"))
	if display == "" {
		display = ":0"
	}

	monitors := enumerateLinuxMonitors(ctx)
	sources := make([]domain.ShareableSource, 0, len(monitors)+8)

	for _, m := range monitors {
		thumb := captureX11RegionThumbnail(ctx, display, m.x, m.y, m.w, m.h, pool)
		if thumb == "" {
			thumb = GeneratePlaceholderThumbnail(m.name, domain.SourceTypeScreen, m.w, m.h)
		}
		sources = append(sources, domain.ShareableSource{
			ID:        m.id,
			Title:     m.name,
			Type:      domain.SourceTypeScreen,
			Thumbnail: thumb,
			Width:     m.w,
			Height:    m.h,
			IsPrimary: m.primary,
		})
	}

	windows := enumerateX11Windows(ctx, display)
	for _, w := range windows {
		id := fmt.Sprintf("window:0x%x", w.xid)
		thumb := captureX11WindowThumbnail(ctx, display, w.xid, w.w, w.h, pool)
		if thumb == "" {
			thumb = GeneratePlaceholderThumbnail(w.title, domain.SourceTypeWindow, w.w, w.h)
		}
		sources = append(sources, domain.ShareableSource{
			ID:        id,
			Title:     w.title,
			Type:      domain.SourceTypeWindow,
			Thumbnail: thumb,
			Width:     w.w,
			Height:    w.h,
		})
	}

	return sources, nil
}

// NewPlatformCapturer creates a Wayland portal or X11 capturer for the requested CaptureConfig.
func NewPlatformCapturer(cfg CaptureConfig, pool *encoder.FramePool) (Capturer, error) {
	if pool == nil {
		pool = encoder.NewFramePool()
	}
	if IsWaylandSession() || cfg.SourceID == "wayland:portal" {
		return newWaylandPortalCapturer(cfg, pool), nil
	}
	return newX11Capturer(cfg, pool), nil
}

func enumerateLinuxMonitors(ctx context.Context) []x11Monitor {
	if xrandrPath, err := exec.LookPath("xrandr"); err == nil {
		qCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		out, err := exec.CommandContext(qCtx, xrandrPath, "--listmonitors").Output()
		cancel()
		if err == nil {
			var monitors []x11Monitor
			scanner := bufio.NewScanner(bytes.NewReader(out))
			idx := 0
			for scanner.Scan() {
				m := xrandrLineRe.FindStringSubmatch(scanner.Text())
				if len(m) != 8 {
					continue
				}
				w, _ := strconv.Atoi(m[3])
				h, _ := strconv.Atoi(m[4])
				x, _ := strconv.Atoi(m[5])
				y, _ := strconv.Atoi(m[6])
				if w <= 0 || h <= 0 {
					continue
				}
				monitors = append(monitors, x11Monitor{
					id:      fmt.Sprintf("screen:%d", idx),
					name:    fmt.Sprintf("Display %d (%s %dx%d)", idx+1, m[7], w, h),
					x:       x,
					y:       y,
					w:       w,
					h:       h,
					primary: m[1] == "*",
				})
				idx++
			}
			if len(monitors) > 0 {
				return monitors
			}
		}
	}

	// Fallback: query X11 root window dimensions directly.
	display := strings.TrimSpace(os.Getenv("DISPLAY"))
	if display != "" {
		if xc, err := dialX11(display); err == nil {
			defer xc.close()
			if _, _, w, h, err := xc.getGeometry(xc.root); err == nil && w > 0 && h > 0 {
				return []x11Monitor{{
					id:      "screen:0",
					name:    fmt.Sprintf("Primary Display (%dx%d)", w, h),
					w:       w,
					h:       h,
					primary: true,
				}}
			}
		}
	}

	return []x11Monitor{{
		id:      "screen:0",
		name:    "Primary Display (1920x1080)",
		w:       1920,
		h:       1080,
		primary: true,
	}}
}

func captureX11RegionThumbnail(ctx context.Context, display string, x, y, w, h int, pool *encoder.FramePool) string {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	cCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(cCtx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "x11grab",
		"-video_size", fmt.Sprintf("%dx%d", w, h),
		"-i", fmt.Sprintf("%s+%d,%d", display, x, y),
		"-frames:v", "1",
		"-vf", "scale=320:-2",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "8",
		"pipe:1",
	).Output()
	if err != nil || len(out) < 4 {
		return ""
	}
	img, _, decErr := image.Decode(bytes.NewReader(out))
	if decErr != nil {
		return ""
	}
	url, _ := ThumbnailJPEGDataURL(img, 320, 180, pool)
	return url
}

func captureX11WindowThumbnail(ctx context.Context, display string, xid uint32, w, h int, pool *encoder.FramePool) string {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	cCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(cCtx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "x11grab",
		"-window_id", fmt.Sprintf("0x%x", xid),
		"-frames:v", "1",
		"-vf", "scale=320:-2",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "8",
		"pipe:1",
	).Output()
	if err != nil || len(out) < 4 {
		return ""
	}
	img, _, decErr := image.Decode(bytes.NewReader(out))
	if decErr != nil {
		return ""
	}
	url, _ := ThumbnailJPEGDataURL(img, 320, 180, pool)
	return url
}

type x11Capturer struct {
	cfg  CaptureConfig
	pool *encoder.FramePool

	framesCh chan FramePacket
	audioCh  chan []byte

	mu          sync.Mutex
	started     bool
	closed      bool
	runErr      error
	cmd         *exec.Cmd
	audioCancel context.CancelFunc
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

func newX11Capturer(cfg CaptureConfig, pool *encoder.FramePool) *x11Capturer {
	var audioCh chan []byte
	if cfg.IncludeSystemAudio {
		audioCh = make(chan []byte, 16)
	}
	return &x11Capturer{
		cfg:      cfg,
		pool:     pool,
		framesCh: make(chan FramePacket, 2),
		audioCh:  audioCh,
	}
}

func (c *x11Capturer) Frames() <-chan FramePacket { return c.framesCh }
func (c *x11Capturer) AudioFrames() <-chan []byte  { return c.audioCh }
func (c *x11Capturer) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runErr
}

func (c *x11Capturer) setErr(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.runErr == nil {
		c.runErr = err
	}
	c.mu.Unlock()
}

func (c *x11Capturer) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("x11 capturer closed")
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
		c.audioCancel = startLinuxLoopbackAudio(runCtx, c.audioCh, &c.wg)
	}

	c.wg.Add(1)
	go c.captureLoop(runCtx)
	return nil
}

func (c *x11Capturer) captureLoop(ctx context.Context) {
	defer c.wg.Done()
	defer c.closeChannels()

	display := strings.TrimSpace(os.Getenv("DISPLAY"))
	if display == "" {
		display = ":0"
	}

	var (
		isWindow bool
		winXID   uint32
		srcX     int
		srcY     int
		srcW     = 1920
		srcH     = 1080
	)

	if strings.HasPrefix(c.cfg.SourceID, "window:") {
		isWindow = true
		parsed, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(c.cfg.SourceID, "window:"), "0x"), 16, 32)
		if err != nil {
			c.setErr(fmt.Errorf("invalid window id %q: %w", c.cfg.SourceID, err))
			return
		}
		winXID = uint32(parsed)
		if xc, err := dialX11(display); err == nil {
			_, _, w, h, gErr := xc.getGeometry(winXID)
			xc.close()
			if gErr != nil {
				c.setErr(ErrSourceClosed)
				return
			}
			srcW, srcH = w, h
		}
	} else {
		monitors := enumerateLinuxMonitors(ctx)
		idx := 0
		if strings.HasPrefix(c.cfg.SourceID, "screen:") {
			if n, err := strconv.Atoi(strings.TrimPrefix(c.cfg.SourceID, "screen:")); err == nil && n >= 0 && n < len(monitors) {
				idx = n
			}
		}
		if idx < len(monitors) {
			srcX, srcY, srcW, srcH = monitors[idx].x, monitors[idx].y, monitors[idx].w, monitors[idx].h
		}
	}

	dstW, dstH := domain.ResolveTargetDimensions(srcW, srcH, c.cfg.Resolution)
	if c.cfg.TargetWidth > 0 && c.cfg.TargetHeight > 0 {
		dstW, dstH = c.cfg.TargetWidth, c.cfg.TargetHeight
	}
	fps := max(1, c.cfg.FPS)

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		c.setErr(errors.New("ffmpeg not found for x11grab capture"))
		return
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "x11grab",
		"-draw_mouse", "1",
		"-framerate", strconv.Itoa(fps),
	}
	if isWindow {
		args = append(args, "-window_id", fmt.Sprintf("0x%x", winXID), "-i", display)
	} else {
		args = append(args, "-video_size", fmt.Sprintf("%dx%d", srcW, srcH), "-i", fmt.Sprintf("%s+%d,%d", display, srcX, srcY))
	}
	args = append(args,
		"-vf", fmt.Sprintf("scale=%d:%d", dstW, dstH),
		"-pix_fmt", "rgba",
		"-f", "rawvideo",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.setErr(err)
		return
	}
	if err := cmd.Start(); err != nil {
		c.setErr(err)
		return
	}
	c.mu.Lock()
	c.cmd = cmd
	c.mu.Unlock()

	if isWindow {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					xc, err := dialX11(display)
					if err != nil {
						continue
					}
					_, _, _, _, gErr := xc.getGeometry(winXID)
					xc.close()
					if gErr != nil {
						c.setErr(ErrSourceClosed)
						if c.cancel != nil {
							c.cancel()
						}
						return
					}
				}
			}
		}()
	}

	frameBytes := dstW * dstH * 4
	reader := bufio.NewReaderSize(stdout, min(frameBytes, 1024*1024))
	for ctx.Err() == nil {
		img := c.pool.GetRGBA(dstW, dstH)
		if _, err := io.ReadFull(reader, img.Pix[:frameBytes]); err != nil {
			c.pool.PutRGBA(img)
			if ctx.Err() == nil {
				c.setErr(ErrSourceClosed)
			}
			break
		}
		pkt := FramePacket{
			Image:      img,
			CapturedAt: time.Now(),
			Release:    func() { c.pool.PutRGBA(img) },
		}
		select {
		case <-ctx.Done():
			pkt.Release()
			return
		case c.framesCh <- pkt:
		default:
			pkt.Release()
		}
	}
	_ = cmd.Wait()
}

func (c *x11Capturer) closeChannels() {
	c.closeOnce.Do(func() {
		close(c.framesCh)
		if c.audioCh != nil {
			close(c.audioCh)
		}
	})
}

func (c *x11Capturer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.wg.Wait()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	audioCancel := c.audioCancel
	cmd := c.cmd
	c.cmd = nil
	c.mu.Unlock()

	if audioCancel != nil {
		audioCancel()
	}
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	c.wg.Wait()
	return nil
}

// startLinuxLoopbackAudio captures the PulseAudio/PipeWire default sink monitor stream at 48kHz stereo.
func startLinuxLoopbackAudio(ctx context.Context, audioCh chan []byte, wg *sync.WaitGroup) context.CancelFunc {
	if audioCh == nil {
		return nil
	}
	parecPath, err := exec.LookPath("parec")
	if err != nil {
		return nil
	}
	monitorDev := detectDefaultMonitorSource(ctx)
	audioCtx, cancel := context.WithCancel(ctx)

	args := []string{
		"--raw",
		"--format=s16le",
		fmt.Sprintf("--rate=%d", encoder.SystemAudioCaptureRate),
		fmt.Sprintf("--channels=%d", encoder.SystemAudioCaptureChannels),
		"--latency-msec=20",
	}
	if monitorDev != "" {
		args = append(args, "--device="+monitorDev)
	}

	cmd := exec.CommandContext(audioCtx, parecPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		cancel()
		return nil
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, encoder.SystemAudioFrameBytes48kStereo)
		for audioCtx.Err() == nil {
			if _, readErr := io.ReadFull(stdout, buf); readErr != nil {
				break
			}
			frameCopy := make([]byte, len(buf))
			copy(frameCopy, buf)
			select {
			case <-audioCtx.Done():
				return
			case audioCh <- frameCopy:
			default:
				// Drop oldest audio chunk if consumer lags to avoid stale audio delay.
				select {
				case <-audioCh:
				default:
				}
				select {
				case audioCh <- frameCopy:
				default:
				}
			}
		}
		_ = cmd.Wait()
	}()
	return cancel
}

func detectDefaultMonitorSource(ctx context.Context) string {
	if pactlPath, err := exec.LookPath("pactl"); err == nil {
		qCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		out, err := exec.CommandContext(qCtx, pactlPath, "get-default-sink").Output()
		cancel()
		if err == nil {
			sink := strings.TrimSpace(string(out))
			if sink != "" {
				return sink + ".monitor"
			}
		}
	}
	return ""
}

// --- Minimal pure-Go X11 wire-protocol client for window discovery (no CGO required) ---

type x11Client struct {
	conn net.Conn
	seq  uint16
	root uint32
}

func enumerateX11Windows(ctx context.Context, display string) []x11Window {
	_ = ctx
	xc, err := dialX11(display)
	if err != nil {
		return nil
	}
	defer xc.close()

	netClientList, err := xc.internAtom("_NET_CLIENT_LIST")
	if err != nil {
		return nil
	}
	netWMName, _ := xc.internAtom("_NET_WM_NAME")
	utf8Str, _ := xc.internAtom("UTF8_STRING")

	rawIDs, err := xc.getProperty(xc.root, netClientList, 33) // XA_WINDOW = 33
	if err != nil || len(rawIDs) < 4 {
		return nil
	}

	var out []x11Window
	for i := 0; i+4 <= len(rawIDs); i += 4 {
		xid := binary.LittleEndian.Uint32(rawIDs[i : i+4])
		if xid == 0 {
			continue
		}
		viewable, err := xc.isViewable(xid)
		if err != nil || !viewable {
			continue
		}
		x, y, w, h, err := xc.getGeometry(xid)
		if err != nil || w < 64 || h < 64 {
			continue
		}
		title := ""
		if netWMName != 0 && utf8Str != 0 {
			if rawName, gErr := xc.getProperty(xid, netWMName, utf8Str); gErr == nil && len(rawName) > 0 {
				title = strings.TrimSpace(strings.TrimRight(string(rawName), "\x00"))
			}
		}
		if title == "" {
			// Fallback to WM_NAME (atom 39, STRING atom 31).
			if rawName, gErr := xc.getProperty(xid, 39, 31); gErr == nil && len(rawName) > 0 {
				title = strings.TrimSpace(strings.TrimRight(string(rawName), "\x00"))
			}
		}
		if title == "" {
			continue
		}
		out = append(out, x11Window{xid: xid, title: title, x: x, y: y, w: w, h: h})
	}
	return out
}

func dialX11(display string) (*x11Client, error) {
	disp := strings.TrimPrefix(display, ":")
	if idx := strings.IndexByte(disp, '.'); idx >= 0 {
		disp = disp[:idx]
	}
	sockPath := filepath.Join("/tmp/.X11-unix", "X"+disp)
	conn, err := net.DialTimeout("unix", sockPath, 600*time.Millisecond)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))

	authName, authData := readXAuthority(disp)
	pad := func(n int) int { return (4 - (n % 4)) % 4 }
	nLen := len(authName)
	dLen := len(authData)
	setup := make([]byte, 12+nLen+pad(nLen)+dLen+pad(dLen))
	setup[0] = 'l' // little-endian
	binary.LittleEndian.PutUint16(setup[2:4], 11)
	binary.LittleEndian.PutUint16(setup[6:8], uint16(nLen))
	binary.LittleEndian.PutUint16(setup[8:10], uint16(dLen))
	off := 12
	copy(setup[off:], authName)
	off += nLen + pad(nLen)
	copy(setup[off:], authData)

	if _, err := conn.Write(setup); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var hdr [8]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	extraLen := int(binary.LittleEndian.Uint16(hdr[6:8])) * 4
	extra := make([]byte, extraLen)
	if _, err := io.ReadFull(conn, extra); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if hdr[0] != 1 {
		_ = conn.Close()
		return nil, errors.New("x11 auth rejected")
	}
	// Root window ID is located after 32 bytes of setup info: 4B release + 4B resBase + 4B resMask + ... -> offset 32 in body (byte 40 overall => extra[32:36]).
	if len(extra) < 36 {
		_ = conn.Close()
		return nil, errors.New("x11 setup response too short")
	}
	vLen := int(binary.LittleEndian.Uint16(extra[16:18]))
	numFormats := int(extra[21])
	screenOff := 32 + vLen + ((4 - (vLen % 4)) % 4) + numFormats*8
	if len(extra) < screenOff+4 {
		_ = conn.Close()
		return nil, errors.New("x11 setup missing screen root")
	}
	root := binary.LittleEndian.Uint32(extra[screenOff : screenOff+4])
	return &x11Client{conn: conn, root: root}, nil
}

func readXAuthority(displayNum string) (string, []byte) {
	path := strings.TrimSpace(os.Getenv("XAUTHORITY"))
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".Xauthority")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	pos := 0
	readBlock := func() ([]byte, bool) {
		if pos+2 > len(raw) {
			return nil, false
		}
		l := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
		pos += 2
		if pos+l > len(raw) {
			return nil, false
		}
		b := raw[pos : pos+l]
		pos += l
		return b, true
	}
	for pos+2 <= len(raw) {
		pos += 2 // family
		if _, ok := readBlock(); !ok {
			break
		}
		numB, ok := readBlock()
		if !ok {
			break
		}
		nameB, ok := readBlock()
		if !ok {
			break
		}
		dataB, ok := readBlock()
		if !ok {
			break
		}
		if len(numB) == 0 || string(numB) == displayNum {
			return string(nameB), dataB
		}
	}
	return "", nil
}

func (x *x11Client) close() {
	if x.conn != nil {
		_ = x.conn.Close()
	}
}

func (x *x11Client) readReply(targetSeq uint16) ([]byte, error) {
	for {
		var hdr [32]byte
		if _, err := io.ReadFull(x.conn, hdr[:]); err != nil {
			return nil, err
		}
		switch hdr[0] {
		case 0: // X11 Error
			return nil, fmt.Errorf("x11 error code %d", hdr[1])
		case 1: // Reply
			extra := int(binary.LittleEndian.Uint32(hdr[4:8])) * 4
			body := make([]byte, extra)
			if extra > 0 {
				if _, err := io.ReadFull(x.conn, body); err != nil {
					return nil, err
				}
			}
			seq := binary.LittleEndian.Uint16(hdr[2:4])
			if seq == targetSeq {
				out := make([]byte, 32+len(body))
				copy(out, hdr[:])
				copy(out[32:], body)
				return out, nil
			}
		default: // Event (32 bytes)
		}
	}
}

func (x *x11Client) internAtom(name string) (uint32, error) {
	pad := (4 - (len(name) % 4)) % 4
	reqLen := (8 + len(name) + pad) / 4
	req := make([]byte, reqLen*4)
	req[0] = 16 // InternAtom
	req[1] = 1  // only-if-exists = true
	binary.LittleEndian.PutUint16(req[2:4], uint16(reqLen))
	binary.LittleEndian.PutUint16(req[4:6], uint16(len(name)))
	copy(req[8:], name)

	x.seq++
	if _, err := x.conn.Write(req); err != nil {
		return 0, err
	}
	rep, err := x.readReply(x.seq)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(rep[8:12]), nil
}

func (x *x11Client) getProperty(win, prop, typ uint32) ([]byte, error) {
	var req [24]byte
	req[0] = 20 // GetProperty
	binary.LittleEndian.PutUint16(req[2:4], 6)
	binary.LittleEndian.PutUint32(req[4:8], win)
	binary.LittleEndian.PutUint32(req[8:12], prop)
	binary.LittleEndian.PutUint32(req[12:16], typ)
	binary.LittleEndian.PutUint32(req[20:24], 4096)

	x.seq++
	if _, err := x.conn.Write(req[:]); err != nil {
		return nil, err
	}
	rep, err := x.readReply(x.seq)
	if err != nil {
		return nil, err
	}
	format := rep[1]
	valueLen := int(binary.LittleEndian.Uint32(rep[16:20]))
	byteLen := valueLen * (int(format) / 8)
	if byteLen <= 0 || 32+byteLen > len(rep) {
		return nil, nil
	}
	return rep[32 : 32+byteLen], nil
}

func (x *x11Client) isViewable(win uint32) (bool, error) {
	var req [8]byte
	req[0] = 3 // GetWindowAttributes
	binary.LittleEndian.PutUint16(req[2:4], 2)
	binary.LittleEndian.PutUint32(req[4:8], win)

	x.seq++
	if _, err := x.conn.Write(req[:]); err != nil {
		return false, err
	}
	rep, err := x.readReply(x.seq)
	if err != nil {
		return false, err
	}
	// map-state at byte offset 26: 2 == IsViewable.
	return rep[26] == 2, nil
}

func (x *x11Client) getGeometry(drawable uint32) (px, py, w, h int, err error) {
	var req [8]byte
	req[0] = 14 // GetGeometry
	binary.LittleEndian.PutUint16(req[2:4], 2)
	binary.LittleEndian.PutUint32(req[4:8], drawable)

	x.seq++
	if _, err := x.conn.Write(req[:]); err != nil {
		return 0, 0, 0, 0, err
	}
	rep, err := x.readReply(x.seq)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	px = int(int16(binary.LittleEndian.Uint16(rep[12:14])))
	py = int(int16(binary.LittleEndian.Uint16(rep[14:16])))
	w = int(binary.LittleEndian.Uint16(rep[16:18]))
	h = int(binary.LittleEndian.Uint16(rep[18:20]))
	return px, py, w, h, nil
}
