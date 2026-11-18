package capture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moscovo/internal/domain"
	"moscovo/internal/screenshare/encoder"
)

type syntheticCapturer struct {
	framesCh chan FramePacket
	audioCh  chan []byte
	mu       sync.Mutex
	runErr   error
	closed   bool
	once     sync.Once
}

func newSyntheticCapturer(withAudio bool) *syntheticCapturer {
	var aud chan []byte
	if withAudio {
		aud = make(chan []byte, 8)
	}
	return &syntheticCapturer{
		framesCh: make(chan FramePacket, 8),
		audioCh:  aud,
	}
}

func (s *syntheticCapturer) Start(context.Context) error { return nil }
func (s *syntheticCapturer) Frames() <-chan FramePacket  { return s.framesCh }
func (s *syntheticCapturer) AudioFrames() <-chan []byte  { return s.audioCh }
func (s *syntheticCapturer) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (s *syntheticCapturer) CloseWithErr(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.runErr = err
		s.closed = true
		s.mu.Unlock()
		close(s.framesCh)
		if s.audioCh != nil {
			close(s.audioCh)
		}
	})
}

func (s *syntheticCapturer) Close() error {
	s.CloseWithErr(nil)
	return nil
}

func TestThumbnailAndPlaceholderGeneration(t *testing.T) {
	pool := encoder.NewFramePool()
	src := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for y := 0; y < 1080; y += 4 {
		for x := 0; x < 1920; x += 4 {
			src.SetRGBA(x, y, color.RGBA{R: 61, G: 220, B: 132, A: 255})
		}
	}

	url, err := ThumbnailJPEGDataURL(src, 320, 180, pool)
	if err != nil {
		t.Fatalf("ThumbnailJPEGDataURL() failed: %v", err)
	}
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("unexpected thumbnail prefix: %q", url[:min(32, len(url))])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("jpeg decode failed: %v", err)
	}
	if decoded.Bounds().Dx() > 320 || decoded.Bounds().Dy() > 180 {
		t.Fatalf("thumbnail dimensions %v exceed 320x180", decoded.Bounds())
	}

	ph := GeneratePlaceholderThumbnail("Editor", domain.SourceTypeWindow, 1280, 720)
	if !strings.HasPrefix(ph, "data:image/jpeg;base64,") {
		t.Fatalf("expected valid placeholder thumbnail")
	}
}

func TestPipelineDropFramePoolReleaseAndAutoStop(t *testing.T) {
	pool := encoder.NewFramePool()
	enc, err := encoder.NewVideoEncoder(encoder.EncoderConfig{
		Width:         640,
		Height:        360,
		FPS:           30,
		BitrateBps:    2_000_000,
		DisableFFmpeg: true,
	})
	if err != nil {
		t.Fatalf("NewVideoEncoder() failed: %v", err)
	}
	audEnc := encoder.NewSystemAudioEncoder(encoder.SystemAudioCaptureRate, encoder.SystemAudioCaptureChannels)
	capImpl := newSyntheticCapturer(true)

	var (
		mu          sync.Mutex
		videoFrames []encoder.EncodedFrame
		audioChunks [][]byte
		stoppedErr  error
	)
	stoppedCh := make(chan struct{})

	pipe, err := NewPipeline(PipelineConfig{
		Capturer:     capImpl,
		Encoder:      enc,
		AudioEncoder: audEnc,
		Pool:         pool,
		TargetWidth:  640,
		TargetHeight: 360,
		OnVideoSample: func(f encoder.EncodedFrame) {
			mu.Lock()
			videoFrames = append(videoFrames, f)
			mu.Unlock()
		},
		OnAudioSample: func(data []byte, _ time.Duration) {
			mu.Lock()
			audioChunks = append(audioChunks, append([]byte(nil), data...))
			mu.Unlock()
		},
		OnStopped: func(reason error) {
			mu.Lock()
			stoppedErr = reason
			mu.Unlock()
			close(stoppedCh)
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline() failed: %v", err)
	}

	if err := pipe.Start(context.Background()); err != nil {
		t.Fatalf("Pipeline.Start() failed: %v", err)
	}

	var released atomic.Int64
	const totalPushed = 6
	for i := 0; i < totalPushed; i++ {
		img := pool.GetRGBA(640, 360)
		capImpl.framesCh <- FramePacket{
			Image:      img,
			CapturedAt: time.Now(),
			Release: func() {
				released.Add(1)
				pool.PutRGBA(img)
			},
		}
	}

	tone := make([]byte, encoder.SystemAudioFrameBytes48kStereo)
	for i := 0; i+4 <= len(tone); i += 4 {
		binary.LittleEndian.PutUint16(tone[i:i+2], uint16(1200))
		binary.LittleEndian.PutUint16(tone[i+2:i+4], uint16(1200))
	}
	capImpl.audioCh <- tone

	// Apply congestion feedback & force keyframe.
	nextBps := pipe.ApplyFeedback(900_000, 0.08, 120)
	if nextBps < domain.MinVideoBitrateBps || nextBps > 900_000 {
		t.Fatalf("unexpected adapted bitrate %d", nextBps)
	}
	pipe.RequestKeyFrame()

	imgLast := pool.GetRGBA(640, 360)
	capImpl.framesCh <- FramePacket{
		Image:      imgLast,
		CapturedAt: time.Now(),
		Release: func() {
			released.Add(1)
			pool.PutRGBA(imgLast)
		},
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		vLen := len(videoFrames)
		aLen := len(audioChunks)
		mu.Unlock()
		if vLen >= 1 && aLen >= 1 && released.Load() == totalPushed+1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if released.Load() != totalPushed+1 {
		t.Fatalf("released packets = %d, want %d (all buffered/dropped packets must release)", released.Load(), totalPushed+1)
	}

	// Simulate shared window closing or Wayland portal revocation -> OnStopped must fire.
	capImpl.CloseWithErr(ErrSourceClosed)

	select {
	case <-stoppedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for automatic OnStopped callback")
	}

	mu.Lock()
	gotReason := stoppedErr
	mu.Unlock()
	if gotReason != ErrSourceClosed {
		t.Fatalf("OnStopped reason = %v, want ErrSourceClosed", gotReason)
	}

	if err := pipe.Stop(); err != nil {
		t.Fatalf("Pipeline.Stop() failed: %v", err)
	}
}

func TestIsWaylandSessionDetection(t *testing.T) {
	if runtime.GOOS != "linux" {
		if IsWaylandSession() {
			t.Fatalf("expected IsWaylandSession() == false on %s", runtime.GOOS)
		}
		return
	}

	t.Setenv("XDG_SESSION_TYPE", "wayland")
	t.Setenv("WAYLAND_DISPLAY", "")
	if !IsWaylandSession() {
		t.Fatalf("expected IsWaylandSession() == true when XDG_SESSION_TYPE=wayland")
	}

	t.Setenv("XDG_SESSION_TYPE", "x11")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if IsWaylandSession() {
		t.Fatalf("expected IsWaylandSession() == false when XDG_SESSION_TYPE=x11")
	}
}
