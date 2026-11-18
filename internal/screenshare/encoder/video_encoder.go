package encoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"moscovo/internal/domain"
)

var (
	// ErrEncoderClosed is returned when Encode is called after Close.
	ErrEncoderClosed = errors.New("video encoder closed")

	vp8LiteMagic = [4]byte{'M', 'V', 'J', 'P'}
)

// EncoderConfig configures a video encoder instance for screen sharing.
type EncoderConfig struct {
	Width          int
	Height         int
	FPS            int
	BitrateBps     int
	CodecMime      string
	PreferHardware bool
	DisableFFmpeg  bool
}

// EncodedFrame holds one encoded video access unit ready for Pion TrackLocalStaticSample.
type EncodedFrame struct {
	Data      []byte
	Duration  time.Duration
	Timestamp time.Time
	KeyFrame  bool
	Width     int
	Height    int
	CodecMime string
}

// VideoEncoder converts YUV420p frames into compressed WebRTC-compatible frames.
type VideoEncoder interface {
	Encode(yuv *YUV420Buffer, forceKeyFrame bool) (EncodedFrame, error)
	SetBitrate(bitrateBps int) error
	Bitrate() int
	CodecMime() string
	Close() error
}

// NewVideoEncoder constructs the best available encoder (hardware FFmpeg -> libvpx -> pure-Go VP8 lite).
func NewVideoEncoder(cfg EncoderConfig) (VideoEncoder, error) {
	cfg = normalizeEncoderConfig(cfg)

	if !cfg.DisableFFmpeg {
		if ffmpegPath, err := exec.LookPath("ffmpeg"); err == nil {
			codecName, mime := selectFFmpegCodec(ffmpegPath, cfg)
			if codecName != "" {
				enc, encErr := newFFmpegVideoEncoder(ffmpegPath, codecName, mime, cfg)
				if encErr == nil {
					return enc, nil
				}
			}
		}
	}

	return newVP8LiteEncoder(cfg), nil
}

func normalizeEncoderConfig(cfg EncoderConfig) EncoderConfig {
	cfg.Width = evenDim(cfg.Width)
	cfg.Height = evenDim(cfg.Height)
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	cfg.BitrateBps = clampBitrate(cfg.BitrateBps)
	return cfg
}

func clampBitrate(bps int) int {
	if bps <= 0 {
		return domain.DefaultVideoBitrateBps
	}
	if bps < domain.MinVideoBitrateBps {
		return domain.MinVideoBitrateBps
	}
	if bps > domain.MaxVideoBitrateBps {
		return domain.MaxVideoBitrateBps
	}
	return bps
}

type codecCandidate struct {
	name string
	mime string
}

func selectFFmpegCodec(ffmpegPath string, cfg EncoderConfig) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", ""
	}
	available := string(out)

	var candidates []codecCandidate
	requestedMime := strings.ToLower(strings.TrimSpace(cfg.CodecMime))

	switch requestedMime {
	case strings.ToLower(webrtc.MimeTypeH264):
		if cfg.PreferHardware {
			candidates = append(candidates,
				codecCandidate{"h264_nvenc", webrtc.MimeTypeH264},
				codecCandidate{"h264_qsv", webrtc.MimeTypeH264},
				codecCandidate{"h264_amf", webrtc.MimeTypeH264},
				codecCandidate{"h264_vaapi", webrtc.MimeTypeH264},
			)
		}
		candidates = append(candidates, codecCandidate{"libx264", webrtc.MimeTypeH264})
	case strings.ToLower(webrtc.MimeTypeVP9):
		if cfg.PreferHardware {
			candidates = append(candidates,
				codecCandidate{"vp9_vaapi", webrtc.MimeTypeVP9},
				codecCandidate{"vp9_qsv", webrtc.MimeTypeVP9},
			)
		}
		candidates = append(candidates, codecCandidate{"libvpx-vp9", webrtc.MimeTypeVP9})
	case strings.ToLower(webrtc.MimeTypeVP8):
		if cfg.PreferHardware {
			candidates = append(candidates, codecCandidate{"vp8_vaapi", webrtc.MimeTypeVP8})
		}
		candidates = append(candidates, codecCandidate{"libvpx", webrtc.MimeTypeVP8})
	default:
		if cfg.PreferHardware {
			candidates = append(candidates,
				codecCandidate{"h264_nvenc", webrtc.MimeTypeH264},
				codecCandidate{"h264_qsv", webrtc.MimeTypeH264},
				codecCandidate{"h264_amf", webrtc.MimeTypeH264},
				codecCandidate{"h264_vaapi", webrtc.MimeTypeH264},
				codecCandidate{"vp8_vaapi", webrtc.MimeTypeVP8},
			)
		}
		candidates = append(candidates,
			codecCandidate{"libvpx", webrtc.MimeTypeVP8},
			codecCandidate{"libx264", webrtc.MimeTypeH264},
		)
	}

	for _, c := range candidates {
		if !strings.Contains(available, " "+c.name+" ") {
			continue
		}
		if probeFFmpegEncoder(ffmpegPath, c.name, cfg.Width, cfg.Height, cfg.FPS) {
			return c.name, c.mime
		}
	}
	return "", ""
}

func probeFFmpegEncoder(ffmpegPath, codecName string, w, h, fps int) bool {
	// Quick 1-frame probe at compact dimensions to confirm runtime GPU/library support.
	pw, ph := 128, 128
	if w >= 64 && h >= 64 {
		pw, ph = 320, 180
	}
	probeCfg := EncoderConfig{
		Width:      pw,
		Height:     ph,
		FPS:        fps,
		BitrateBps: domain.MinVideoBitrateBps,
	}
	args := buildFFmpegEncoderArgs(codecName, probeCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	frame := make([]byte, pw*ph*3/2)
	cmd.Stdin = bytes.NewReader(frame)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	return out.Len() > 16
}

func buildFFmpegEncoderArgs(codecName string, cfg EncoderConfig) []string {
	bitrateKbps := fmt.Sprintf("%dk", max(100, cfg.BitrateBps/1000))
	sizeArg := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	fpsArg := fmt.Sprintf("%d", cfg.FPS)
	gopArg := fmt.Sprintf("%d", max(15, cfg.FPS*2))

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-s", sizeArg,
		"-r", fpsArg,
		"-i", "pipe:0",
	}

	switch codecName {
	case "h264_vaapi":
		args = append(args,
			"-vaapi_device", "/dev/dri/renderD128",
			"-vf", "format=nv12,hwupload",
			"-c:v", "h264_vaapi",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "flv", "-flush_packets", "1", "pipe:1",
		)
	case "vp8_vaapi":
		args = append(args,
			"-vaapi_device", "/dev/dri/renderD128",
			"-vf", "format=nv12,hwupload",
			"-c:v", "vp8_vaapi",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "ivf", "-flush_packets", "1", "pipe:1",
		)
	case "vp9_vaapi":
		args = append(args,
			"-vaapi_device", "/dev/dri/renderD128",
			"-vf", "format=nv12,hwupload",
			"-c:v", "vp9_vaapi",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "ivf", "-flush_packets", "1", "pipe:1",
		)
	case "h264_nvenc":
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "p1",
			"-tune", "ull",
			"-zerolatency", "1",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "flv", "-flush_packets", "1", "pipe:1",
		)
	case "h264_qsv":
		args = append(args,
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "flv", "-flush_packets", "1", "pipe:1",
		)
	case "h264_amf":
		args = append(args,
			"-c:v", "h264_amf",
			"-usage", "ultralowlatency",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "flv", "-flush_packets", "1", "pipe:1",
		)
	case "libx264":
		args = append(args,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-b:v", bitrateKbps,
			"-maxrate", bitrateKbps,
			"-bufsize", bitrateKbps,
			"-g", gopArg,
			"-f", "flv", "-flush_packets", "1", "pipe:1",
		)
	case "libvpx-vp9", "vp9_qsv":
		args = append(args,
			"-c:v", codecName,
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-threads", "4",
			"-lag-in-frames", "0",
			"-b:v", bitrateKbps,
			"-g", gopArg,
			"-f", "ivf", "-flush_packets", "1", "pipe:1",
		)
	default: // libvpx
		args = append(args,
			"-c:v", "libvpx",
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-threads", "4",
			"-lag-in-frames", "0",
			"-error-resilient", "1",
			"-b:v", bitrateKbps,
			"-maxrate", bitrateKbps,
			"-bufsize", bitrateKbps,
			"-g", gopArg,
			"-f", "ivf", "-flush_packets", "1", "pipe:1",
		)
	}

	return args
}

// ffmpegVideoEncoder streams raw YUV420p frames into a persistent ffmpeg child process.
type ffmpegVideoEncoder struct {
	mu            sync.Mutex
	ffmpegPath    string
	codecName     string
	codecMime     string
	cfg           EncoderConfig
	appliedBps    int
	headerRead    bool
	closed        bool
	planarScratch []byte

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	spsPPS []byte // cached Annex-B SPS+PPS for H.264 keyframes
}

func newFFmpegVideoEncoder(ffmpegPath, codecName, codecMime string, cfg EncoderConfig) (*ffmpegVideoEncoder, error) {
	enc := &ffmpegVideoEncoder{
		ffmpegPath: ffmpegPath,
		codecName:  codecName,
		codecMime:  codecMime,
		cfg:        cfg,
		appliedBps: cfg.BitrateBps,
	}
	if err := enc.startProcessLocked(); err != nil {
		return nil, err
	}
	return enc, nil
}

func (e *ffmpegVideoEncoder) startProcessLocked() error {
	args := buildFFmpegEncoderArgs(e.codecName, e.cfg)
	cmd := exec.Command(e.ffmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	e.cmd = cmd
	e.stdin = stdin
	e.stdout = bufio.NewReaderSize(stdoutPipe, 64*1024)
	e.appliedBps = e.cfg.BitrateBps
	e.headerRead = false
	e.spsPPS = nil
	return nil
}

func (e *ffmpegVideoEncoder) stopProcessLocked() {
	if e.stdin != nil {
		_ = e.stdin.Close()
		e.stdin = nil
	}
	if e.cmd != nil {
		cmd := e.cmd
		e.cmd = nil
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	}
	e.stdout = nil
}

func (e *ffmpegVideoEncoder) CodecMime() string {
	return e.codecMime
}

func (e *ffmpegVideoEncoder) Bitrate() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.BitrateBps
}

func (e *ffmpegVideoEncoder) SetBitrate(bitrateBps int) error {
	clamped := clampBitrate(bitrateBps)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEncoderClosed
	}
	e.cfg.BitrateBps = clamped
	// Restart only on significant bitrate shifts (>= 20%) to avoid unnecessary keyframe hitches.
	diff := clamped - e.appliedBps
	if diff < 0 {
		diff = -diff
	}
	if e.appliedBps > 0 && diff*5 >= e.appliedBps {
		e.stopProcessLocked()
		return e.startProcessLocked()
	}
	return nil
}

func (e *ffmpegVideoEncoder) Encode(yuv *YUV420Buffer, forceKeyFrame bool) (EncodedFrame, error) {
	if yuv == nil {
		return EncodedFrame{}, errors.New("yuv frame cannot be nil")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return EncodedFrame{}, ErrEncoderClosed
	}

	if forceKeyFrame && e.cmd != nil {
		// Restarting the realtime pipe guarantees an immediate keyframe on PLI/FIR without waiting for GOP.
		e.stopProcessLocked()
		if err := e.startProcessLocked(); err != nil {
			return EncodedFrame{}, err
		}
	}
	if e.cmd == nil {
		if err := e.startProcessLocked(); err != nil {
			return EncodedFrame{}, err
		}
	}

	e.planarScratch = yuv.WritePlanar(e.planarScratch)
	if _, err := e.stdin.Write(e.planarScratch); err != nil {
		return EncodedFrame{}, fmt.Errorf("write yuv frame to ffmpeg: %w", err)
	}

	if !e.headerRead {
		if e.codecMime == webrtc.MimeTypeH264 {
			var hdr [13]byte
			if _, err := io.ReadFull(e.stdout, hdr[:]); err != nil {
				return EncodedFrame{}, fmt.Errorf("read flv header: %w", err)
			}
		} else {
			var hdr [32]byte
			if _, err := io.ReadFull(e.stdout, hdr[:]); err != nil {
				return EncodedFrame{}, fmt.Errorf("read ivf header: %w", err)
			}
		}
		e.headerRead = true
	}

	var (
		data     []byte
		keyFrame bool
		err      error
	)
	if e.codecMime == webrtc.MimeTypeH264 {
		data, keyFrame, err = e.readFLVVideoFrameLocked()
	} else {
		data, keyFrame, err = e.readIVFFrameLocked()
	}
	if err != nil {
		return EncodedFrame{}, err
	}

	return EncodedFrame{
		Data:      data,
		Duration:  time.Second / time.Duration(max(1, e.cfg.FPS)),
		Timestamp: time.Now(),
		KeyFrame:  keyFrame,
		Width:     e.cfg.Width,
		Height:    e.cfg.Height,
		CodecMime: e.codecMime,
	}, nil
}

func (e *ffmpegVideoEncoder) readIVFFrameLocked() ([]byte, bool, error) {
	var fhdr [12]byte
	if _, err := io.ReadFull(e.stdout, fhdr[:]); err != nil {
		return nil, false, fmt.Errorf("read ivf frame header: %w", err)
	}
	sz := binary.LittleEndian.Uint32(fhdr[:4])
	if sz == 0 || sz > 16*1024*1024 {
		return nil, false, fmt.Errorf("invalid ivf frame size %d", sz)
	}
	buf := make([]byte, sz)
	if _, err := io.ReadFull(e.stdout, buf); err != nil {
		return nil, false, fmt.Errorf("read ivf frame payload: %w", err)
	}
	keyFrame := len(buf) > 0 && (buf[0]&0x01) == 0
	return buf, keyFrame, nil
}

func (e *ffmpegVideoEncoder) readFLVVideoFrameLocked() ([]byte, bool, error) {
	for {
		var th [11]byte
		if _, err := io.ReadFull(e.stdout, th[:]); err != nil {
			return nil, false, fmt.Errorf("read flv tag header: %w", err)
		}
		tagType := th[0]
		sz := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		if sz < 0 || sz > 16*1024*1024 {
			return nil, false, fmt.Errorf("invalid flv tag size %d", sz)
		}
		payload := make([]byte, sz)
		if _, err := io.ReadFull(e.stdout, payload); err != nil {
			return nil, false, fmt.Errorf("read flv tag payload: %w", err)
		}
		var prev [4]byte
		if _, err := io.ReadFull(e.stdout, prev[:]); err != nil {
			return nil, false, fmt.Errorf("read flv prev tag size: %w", err)
		}
		if tagType != 9 || len(payload) < 5 {
			continue
		}
		frameType := (payload[0] >> 4) & 0x0f
		avcPacketType := payload[1]
		if avcPacketType == 0 {
			e.spsPPS = parseAVCDecoderConfigToAnnexB(payload[5:])
			continue
		}
		if avcPacketType != 1 {
			continue
		}
		annexB := avccToAnnexB(payload[5:])
		keyFrame := frameType == 1
		if keyFrame && len(e.spsPPS) > 0 {
			combined := make([]byte, len(e.spsPPS)+len(annexB))
			copy(combined, e.spsPPS)
			copy(combined[len(e.spsPPS):], annexB)
			annexB = combined
		}
		return annexB, keyFrame, nil
	}
}

func (e *ffmpegVideoEncoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.stopProcessLocked()
	return nil
}

func parseAVCDecoderConfigToAnnexB(cfg []byte) []byte {
	// AVCDecoderConfigurationRecord layout: 5 bytes header, numSPS (5 bits), [len(2B)+NAL]...
	if len(cfg) < 7 {
		return nil
	}
	var out bytes.Buffer
	pos := 5
	numSPS := int(cfg[pos] & 0x1f)
	pos++
	for i := 0; i < numSPS && pos+2 <= len(cfg); i++ {
		nalLen := int(binary.BigEndian.Uint16(cfg[pos : pos+2]))
		pos += 2
		if nalLen <= 0 || pos+nalLen > len(cfg) {
			return out.Bytes()
		}
		out.Write([]byte{0x00, 0x00, 0x00, 0x01})
		out.Write(cfg[pos : pos+nalLen])
		pos += nalLen
	}
	if pos >= len(cfg) {
		return out.Bytes()
	}
	numPPS := int(cfg[pos])
	pos++
	for i := 0; i < numPPS && pos+2 <= len(cfg); i++ {
		nalLen := int(binary.BigEndian.Uint16(cfg[pos : pos+2]))
		pos += 2
		if nalLen <= 0 || pos+nalLen > len(cfg) {
			return out.Bytes()
		}
		out.Write([]byte{0x00, 0x00, 0x00, 0x01})
		out.Write(cfg[pos : pos+nalLen])
		pos += nalLen
	}
	return out.Bytes()
}

func avccToAnnexB(avcc []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(avcc) + 16)
	pos := 0
	for pos+4 <= len(avcc) {
		nalLen := int(binary.BigEndian.Uint32(avcc[pos : pos+4]))
		pos += 4
		if nalLen <= 0 || pos+nalLen > len(avcc) {
			break
		}
		out.Write([]byte{0x00, 0x00, 0x00, 0x01})
		out.Write(avcc[pos : pos+nalLen])
		pos += nalLen
	}
	return out.Bytes()
}

// vp8LiteEncoder is a zero-dependency pure-Go low-latency VP8-compatible frame encoder.
// It emits RFC 6386 VP8 uncompressed frame headers encapsulating bitrate-adaptive JPEG frames
// so Pion TrackLocalStaticSample (MimeTypeVP8) packetizes cleanly without external binaries.
type vp8LiteEncoder struct {
	mu         sync.Mutex
	cfg        EncoderConfig
	frameCount uint64
	closed     bool
}

func newVP8LiteEncoder(cfg EncoderConfig) *vp8LiteEncoder {
	return &vp8LiteEncoder{
		cfg: cfg,
	}
}

func (e *vp8LiteEncoder) CodecMime() string {
	return webrtc.MimeTypeVP8
}

func (e *vp8LiteEncoder) Bitrate() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.BitrateBps
}

func (e *vp8LiteEncoder) SetBitrate(bitrateBps int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEncoderClosed
	}
	e.cfg.BitrateBps = clampBitrate(bitrateBps)
	return nil
}

func (e *vp8LiteEncoder) Encode(yuv *YUV420Buffer, forceKeyFrame bool) (EncodedFrame, error) {
	if yuv == nil {
		return EncodedFrame{}, errors.New("yuv frame cannot be nil")
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return EncodedFrame{}, ErrEncoderClosed
	}
	fps := max(1, e.cfg.FPS)
	bps := e.cfg.BitrateBps
	keyFrame := forceKeyFrame || e.frameCount == 0 || (e.frameCount%uint64(fps*2)) == 0
	e.frameCount++
	e.mu.Unlock()

	ycbcr := &image.YCbCr{
		Y:              yuv.Y,
		Cb:             yuv.U,
		Cr:             yuv.V,
		YStride:        yuv.YStride,
		CStride:        yuv.UStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, yuv.Width, yuv.Height),
	}

	quality := bitrateToJPEGQuality(bps)
	jpegBytes, err := EncodeRGBAToJPEG(ycbcr, quality)
	if err != nil {
		return EncodedFrame{}, err
	}

	payload := wrapVP8LiteFrame(jpegBytes, yuv.Width, yuv.Height, keyFrame)
	return EncodedFrame{
		Data:      payload,
		Duration:  time.Second / time.Duration(fps),
		Timestamp: time.Now(),
		KeyFrame:  keyFrame,
		Width:     yuv.Width,
		Height:    yuv.Height,
		CodecMime: webrtc.MimeTypeVP8,
	}, nil
}

func (e *vp8LiteEncoder) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}

func bitrateToJPEGQuality(bps int) int {
	// Map [500 kbps, 4 Mbps] -> JPEG quality [35, 82].
	if bps <= domain.MinVideoBitrateBps {
		return 35
	}
	if bps >= domain.MaxVideoBitrateBps {
		return 82
	}
	num := (bps - domain.MinVideoBitrateBps) * (82 - 35)
	den := domain.MaxVideoBitrateBps - domain.MinVideoBitrateBps
	return 35 + num/den
}

func wrapVP8LiteFrame(jpegData []byte, width, height int, keyFrame bool) []byte {
	// RFC 6386 VP8 uncompressed chunk: 3-byte frame tag, plus 7-byte keyframe header when keyFrame=true.
	// Bit 0 of frame tag is 0 for keyframe, 1 for interframe; bit 4 is show_frame = 1.
	hdrLen := 3
	if keyFrame {
		hdrLen = 10
	}
	firstPartSize := uint32(len(jpegData) + 4)
	if firstPartSize > 0x7ffff {
		firstPartSize = 0x7ffff
	}

	tag := (firstPartSize << 5) | (1 << 4)
	if !keyFrame {
		tag |= 1
	}

	out := make([]byte, hdrLen+4+len(jpegData))
	out[0] = byte(tag)
	out[1] = byte(tag >> 8)
	out[2] = byte(tag >> 16)
	off := 3
	if keyFrame {
		out[3] = 0x9d
		out[4] = 0x01
		out[5] = 0x2a
		binary.LittleEndian.PutUint16(out[6:8], uint16(width&0x3fff))
		binary.LittleEndian.PutUint16(out[8:10], uint16(height&0x3fff))
		off = 10
	}
	copy(out[off:off+4], vp8LiteMagic[:])
	copy(out[off+4:], jpegData)
	return out
}

// DecodeVP8FrameToJPEG extracts embedded JPEG data from a vp8LiteEncoder frame if present.
func DecodeVP8FrameToJPEG(payload []byte) (jpegData []byte, width int, height int, ok bool) {
	if len(payload) < 10 {
		return nil, 0, 0, false
	}
	isInter := (payload[0] & 0x01) != 0
	off := 3
	if !isInter {
		if len(payload) < 14 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return nil, 0, 0, false
		}
		width = int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
		height = int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
		off = 10
	}
	if len(payload) <= off+4 || !bytes.Equal(payload[off:off+4], vp8LiteMagic[:]) {
		return nil, 0, 0, false
	}
	jpegBytes := append([]byte(nil), payload[off+4:]...)
	return jpegBytes, width, height, true
}
