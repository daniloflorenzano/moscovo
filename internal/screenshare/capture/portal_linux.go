//go:build linux

package capture

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

const (
	portalDest            = "org.freedesktop.portal.Desktop"
	portalObjectPath      = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalScreenCastIface = "org.freedesktop.portal.ScreenCast"
	portalRequestIface    = "org.freedesktop.portal.Request"
	portalSessionIface    = "org.freedesktop.portal.Session"
)

var portalTokenCounter atomic.Uint64

// IsWaylandSession reports whether the active Linux desktop session is running under Wayland.
func IsWaylandSession() bool {
	sessionType := strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE"))
	if sessionType != "" {
		return strings.EqualFold(sessionType, "wayland")
	}
	return strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != ""
}

type portalStreamInfo struct {
	nodeID uint32
	width  int
	height int
}

type waylandPortalCapturer struct {
	cfg  CaptureConfig
	pool *encoder.FramePool

	framesCh chan FramePacket
	audioCh  chan []byte

	mu      sync.Mutex
	started bool
	closed  bool
	runErr  error

	dbusConn      *dbus.Conn
	sessionHandle dbus.ObjectPath
	pwFile        *os.File
	cmd           *exec.Cmd
	audioCancel   context.CancelFunc

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newWaylandPortalCapturer(cfg CaptureConfig, pool *encoder.FramePool) *waylandPortalCapturer {
	if pool == nil {
		pool = encoder.NewFramePool()
	}
	var audioCh chan []byte
	if cfg.IncludeSystemAudio {
		audioCh = make(chan []byte, 16)
	}
	return &waylandPortalCapturer{
		cfg:      cfg,
		pool:     pool,
		framesCh: make(chan FramePacket, 2),
		audioCh:  audioCh,
	}
}

func (c *waylandPortalCapturer) Frames() <-chan FramePacket {
	return c.framesCh
}

func (c *waylandPortalCapturer) AudioFrames() <-chan []byte {
	return c.audioCh
}

func (c *waylandPortalCapturer) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runErr
}

func (c *waylandPortalCapturer) setErr(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.runErr == nil {
		c.runErr = err
	}
	c.mu.Unlock()
}

func (c *waylandPortalCapturer) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("wayland portal capturer closed")
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

	// Start isolated desktop loopback audio capture if requested (FR-06).
	if c.cfg.IncludeSystemAudio {
		c.audioCancel = startLinuxLoopbackAudio(runCtx, c.audioCh, &c.wg)
	}

	conn, sessionHandle, stream, pwFile, err := c.negotiatePortal(runCtx)
	if err != nil {
		// On wlroots/Hyprland compositors without xdg-desktop-portal-wlr/pipewire, fall back to grim if available.
		if !errors.Is(err, ErrPortalCancelled) {
			if grimPath, gErr := exec.LookPath("grim"); gErr == nil {
				c.wg.Add(1)
				go c.runGrimFallbackLoop(runCtx, grimPath)
				return nil
			}
		}
		cancel()
		c.closeChannels()
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = pwFile.Close()
		_ = conn.Close()
		cancel()
		return errors.New("wayland portal capturer closed")
	}
	c.dbusConn = conn
	c.sessionHandle = sessionHandle
	c.pwFile = pwFile
	c.mu.Unlock()

	dstW, dstH := domain.ResolveTargetDimensions(stream.width, stream.height, c.cfg.Resolution)
	if c.cfg.TargetWidth > 0 && c.cfg.TargetHeight > 0 {
		dstW, dstH = c.cfg.TargetWidth, c.cfg.TargetHeight
	}

	c.watchPortalSessionClosed(runCtx, conn, sessionHandle)

	c.wg.Add(1)
	go c.runPipeWireStream(runCtx, stream.nodeID, pwFile, dstW, dstH)
	return nil
}

func (c *waylandPortalCapturer) negotiatePortal(ctx context.Context) (*dbus.Conn, dbus.ObjectPath, portalStreamInfo, *os.File, error) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("connect session dbus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("auth session dbus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("dbus hello: %w", err)
	}

	names := conn.Names()
	if len(names) == 0 {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, errors.New("dbus connection missing unique name")
	}
	sender := strings.ReplaceAll(strings.TrimPrefix(names[0], ":"), ".", "_")
	portalObj := conn.Object(portalDest, portalObjectPath)

	// 1. CreateSession
	sessToken := fmt.Sprintf("moscovo_sess_%d", portalTokenCounter.Add(1))
	createRes, err := callPortalMethod(ctx, conn, sender, func(reqToken string) error {
		opts := map[string]dbus.Variant{
			"handle_token":         dbus.MakeVariant(reqToken),
			"session_handle_token": dbus.MakeVariant(sessToken),
		}
		return portalObj.CallWithContext(ctx, portalScreenCastIface+".CreateSession", 0, opts).Err
	})
	if err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("portal CreateSession: %w", err)
	}

	var sessionHandle dbus.ObjectPath
	if shVar, ok := createRes["session_handle"]; ok {
		switch v := shVar.Value().(type) {
		case string:
			sessionHandle = dbus.ObjectPath(v)
		case dbus.ObjectPath:
			sessionHandle = v
		}
	}
	if sessionHandle == "" {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, errors.New("portal CreateSession missing session_handle")
	}

	// 2. SelectSources (monitors | windows, embedded cursor)
	_, err = callPortalMethod(ctx, conn, sender, func(reqToken string) error {
		opts := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(reqToken),
			"types":        dbus.MakeVariant(uint32(1 | 2)),
			"multiple":     dbus.MakeVariant(false),
			"cursor_mode":  dbus.MakeVariant(uint32(2)),
			"persist_mode": dbus.MakeVariant(uint32(0)),
		}
		return portalObj.CallWithContext(ctx, portalScreenCastIface+".SelectSources", 0, sessionHandle, opts).Err
	})
	if err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("portal SelectSources: %w", err)
	}

	// 3. Start (presents native portal picker to the user)
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()
	startRes, err := callPortalMethod(startCtx, conn, sender, func(reqToken string) error {
		opts := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(reqToken),
		}
		return portalObj.CallWithContext(startCtx, portalScreenCastIface+".Start", 0, sessionHandle, "", opts).Err
	})
	if err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, err
	}

	stream, err := extractPortalStream(startRes)
	if err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, err
	}

	// 4. OpenPipeWireRemote -> UnixFD
	var pwFD dbus.UnixFD
	if err := portalObj.CallWithContext(ctx, portalScreenCastIface+".OpenPipeWireRemote", 0, sessionHandle, map[string]dbus.Variant{}).Store(&pwFD); err != nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, fmt.Errorf("portal OpenPipeWireRemote: %w", err)
	}
	pwFile := os.NewFile(uintptr(pwFD), "pipewire-remote")
	if pwFile == nil {
		_ = conn.Close()
		return nil, "", portalStreamInfo{}, nil, errors.New("invalid pipewire file descriptor")
	}

	return conn, sessionHandle, stream, pwFile, nil
}

func callPortalMethod(
	ctx context.Context,
	conn *dbus.Conn,
	sender string,
	invoke func(reqToken string) error,
) (map[string]dbus.Variant, error) {
	reqToken := fmt.Sprintf("moscovo_req_%d", portalTokenCounter.Add(1))
	reqPath := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/request/%s/%s", sender, reqToken))

	matchOpts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(reqPath),
		dbus.WithMatchInterface(portalRequestIface),
		dbus.WithMatchMember("Response"),
	}
	if err := conn.AddMatchSignal(matchOpts...); err != nil {
		return nil, err
	}
	defer func() { _ = conn.RemoveMatchSignal(matchOpts...) }()

	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	if err := invoke(reqToken); err != nil {
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sig := <-sigCh:
			if sig == nil || sig.Path != reqPath || sig.Name != portalRequestIface+".Response" {
				continue
			}
			if len(sig.Body) < 2 {
				return nil, errors.New("malformed portal Response signal")
			}
			code, _ := sig.Body[0].(uint32)
			if code == 1 {
				return nil, ErrPortalCancelled
			}
			if code != 0 {
				return nil, fmt.Errorf("portal request failed with code %d", code)
			}
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			return results, nil
		}
	}
}

func extractPortalStream(results map[string]dbus.Variant) (portalStreamInfo, error) {
	rawStreams, ok := results["streams"]
	if !ok {
		return portalStreamInfo{}, errors.New("portal Start returned no streams")
	}
	streams, ok := rawStreams.Value().([][]any)
	if !ok || len(streams) == 0 {
		return portalStreamInfo{}, errors.New("portal Start returned empty stream list")
	}
	first := streams[0]
	if len(first) < 1 {
		return portalStreamInfo{}, errors.New("malformed portal stream entry")
	}
	nodeID, ok := first[0].(uint32)
	if !ok {
		return portalStreamInfo{}, errors.New("portal stream missing PipeWire node_id")
	}

	info := portalStreamInfo{nodeID: nodeID, width: 1920, height: 1080}
	if len(first) >= 2 {
		if props, ok := first[1].(map[string]dbus.Variant); ok {
			if sizeVar, hasSize := props["size"]; hasSize {
				if dims, ok := sizeVar.Value().([]any); ok && len(dims) == 2 {
					if w, ok := dims[0].(int32); ok && w > 0 {
						info.width = int(w)
					}
					if h, ok := dims[1].(int32); ok && h > 0 {
						info.height = int(h)
					}
				}
			}
		}
	}
	return info, nil
}

func (c *waylandPortalCapturer) watchPortalSessionClosed(ctx context.Context, conn *dbus.Conn, sessionHandle dbus.ObjectPath) {
	matchOpts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(sessionHandle),
		dbus.WithMatchInterface(portalSessionIface),
		dbus.WithMatchMember("Closed"),
	}
	_ = conn.AddMatchSignal(matchOpts...)
	sigCh := make(chan *dbus.Signal, 2)
	conn.Signal(sigCh)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			conn.RemoveSignal(sigCh)
			_ = conn.RemoveMatchSignal(matchOpts...)
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if sig != nil && sig.Path == sessionHandle && sig.Name == portalSessionIface+".Closed" {
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

func (c *waylandPortalCapturer) runPipeWireStream(ctx context.Context, nodeID uint32, pwFile *os.File, w, h int) {
	defer c.wg.Done()
	defer c.closeChannels()

	gstPath, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if grimPath, gErr := exec.LookPath("grim"); gErr == nil {
			c.runGrimLoopInline(ctx, grimPath)
			return
		}
		c.setErr(errors.New("gst-launch-1.0 not found for PipeWire capture"))
		return
	}

	fps := max(1, c.cfg.FPS)
	// ExtraFiles[0] is mapped to file descriptor 3 inside the child process.
	pipelineArgs := []string{
		"-q",
		"pipewiresrc", "fd=3", "path=" + strconv.FormatUint(uint64(nodeID), 10), "do-timestamp=true",
		"!", "videoconvert",
		"!", "videoscale",
		"!", fmt.Sprintf("video/x-raw,format=RGBA,width=%d,height=%d,framerate=%d/1", w, h, fps),
		"!", "fdsink", "fd=1",
	}

	cmd := exec.CommandContext(ctx, gstPath, pipelineArgs...)
	cmd.ExtraFiles = []*os.File{pwFile}
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

	frameBytes := w * h * 4
	reader := bufio.NewReaderSize(stdout, min(frameBytes, 1024*1024))
	for ctx.Err() == nil {
		img := c.pool.GetRGBA(w, h)
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

func (c *waylandPortalCapturer) runGrimFallbackLoop(ctx context.Context, grimPath string) {
	defer c.wg.Done()
	defer c.closeChannels()
	c.runGrimLoopInline(ctx, grimPath)
}

func (c *waylandPortalCapturer) runGrimLoopInline(ctx context.Context, grimPath string) {
	interval := time.Second / time.Duration(max(1, c.cfg.FPS))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			img, err := captureGrimRGBA(ctx, grimPath, c.pool)
			if err != nil {
				continue
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
	}
}

func captureGrimRGBA(ctx context.Context, grimPath string, pool *encoder.FramePool) (*image.RGBA, error) {
	gCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(gCtx, grimPath, "-t", "png", "-").Output()
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	b := decoded.Bounds()
	img := pool.GetRGBA(b.Dx(), b.Dy())
	if src, ok := decoded.(*image.RGBA); ok {
		copy(img.Pix, src.Pix)
		return img, nil
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := decoded.At(b.Min.X+x, b.Min.Y+y).RGBA()
			off := y*img.Stride + x*4
			img.Pix[off] = uint8(r >> 8)
			img.Pix[off+1] = uint8(g >> 8)
			img.Pix[off+2] = uint8(bl >> 8)
			img.Pix[off+3] = 255
		}
	}
	return img, nil
}

func (c *waylandPortalCapturer) closeChannels() {
	c.closeOnce.Do(func() {
		close(c.framesCh)
		if c.audioCh != nil {
			close(c.audioCh)
		}
	})
}

func (c *waylandPortalCapturer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.wg.Wait()
		return nil
	}
	c.closed = true
	cancel := c.cancel
	audioCancel := c.audioCancel
	pwFile := c.pwFile
	conn := c.dbusConn
	sess := c.sessionHandle
	cmd := c.cmd
	c.pwFile = nil
	c.dbusConn = nil
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
	if pwFile != nil {
		_ = pwFile.Close()
	}
	if conn != nil {
		if sess != "" {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_ = conn.Object(portalDest, sess).CallWithContext(closeCtx, portalSessionIface+".Close", 0).Err
			closeCancel()
		}
		_ = conn.Close()
	}
	c.wg.Wait()
	return nil
}
