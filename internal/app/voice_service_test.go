package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"moscovo/internal/domain"
	"moscovo/internal/hub"
	"moscovo/internal/screenshare/capture"
	"moscovo/internal/screenshare/encoder"
)

type stubCapturer struct {
	framesCh chan capture.FramePacket
	audioCh  chan []byte
	mu       sync.Mutex
	runErr   error
	once     sync.Once
}

func newStubCapturer(withAudio bool) *stubCapturer {
	var aud chan []byte
	if withAudio {
		aud = make(chan []byte, 8)
	}
	return &stubCapturer{
		framesCh: make(chan capture.FramePacket, 4),
		audioCh:  aud,
	}
}

func (s *stubCapturer) Start(context.Context) error        { return nil }
func (s *stubCapturer) Frames() <-chan capture.FramePacket { return s.framesCh }
func (s *stubCapturer) AudioFrames() <-chan []byte         { return s.audioCh }
func (s *stubCapturer) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (s *stubCapturer) ForceCloseWithErr(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
		close(s.framesCh)
		if s.audioCh != nil {
			close(s.audioCh)
		}
	})
}

func (s *stubCapturer) Close() error {
	s.ForceCloseWithErr(nil)
	return nil
}

func startLocalTestHub(t *testing.T) string {
	t.Helper()
	hubSrv := hub.NewServer(hub.ServerConfig{
		PingInterval: 2 * time.Second,
		PongWait:     6 * time.Second,
		WriteWait:    2 * time.Second,
	})
	httpSrv := httptest.NewServer(hubSrv.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hubSrv.Shutdown(ctx)
	})
	return httpSrv.URL
}

func TestVoiceServiceScreenShareLifecycleAndAutoStopEvent(t *testing.T) {
	hubURL := startLocalTestHub(t)

	var (
		evMu      sync.Mutex
		stoppedEv []map[string]any
		lastCap   *stubCapturer
	)

	svc := NewVoiceService(ServiceConfig{
		HubAddr:         hubURL,
		Nickname:        "tester",
		STUNServers:     []string{},
		IncludeLoopback: true,
		Emit: func(name string, data any) {
			if name == EventScreenShareStopped {
				if m, ok := data.(map[string]any); ok {
					evMu.Lock()
					stoppedEv = append(stoppedEv, m)
					evMu.Unlock()
				}
			}
		},
	})
	svc.disableFFmpeg = true
	svc.sourcesFetcher = func(context.Context) ([]domain.ShareableSource, error) {
		return []domain.ShareableSource{
			{
				ID:        "screen:0",
				Title:     "Primary Display",
				Type:      domain.SourceTypeScreen,
				Thumbnail: capture.GeneratePlaceholderThumbnail("Primary Display", domain.SourceTypeScreen, 1280, 720),
				Width:     1280,
				Height:    720,
				IsPrimary: true,
			},
		}, nil
	}
	svc.capturerFactory = func(cfg capture.CaptureConfig, pool *encoder.FramePool) (capture.Capturer, error) {
		c := newStubCapturer(cfg.IncludeSystemAudio)
		evMu.Lock()
		lastCap = c
		evMu.Unlock()
		return c, nil
	}
	defer func() { _ = svc.Shutdown() }()

	// Cannot start screen share before joining a voice call.
	if _, err := svc.StartScreenShare(domain.ScreenShareConfig{
		SourceID:   "screen:0",
		Resolution: domain.Resolution720p,
		FPS:        30,
	}); err == nil {
		t.Fatalf("expected StartScreenShare outside call to fail")
	}

	sources, err := svc.GetShareableSources()
	if err != nil || len(sources) != 1 || sources[0].ID != "screen:0" {
		t.Fatalf("GetShareableSources() = (%v, %v), want 1 source", sources, err)
	}

	if _, err := svc.JoinCall(); err != nil {
		t.Fatalf("JoinCall() failed: %v", err)
	}

	// Concurrent rapid StartScreenShare attempts: exactly one should succeed (NFR-06).
	var wg sync.WaitGroup
	var okMu sync.Mutex
	okCount := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, startErr := svc.StartScreenShare(domain.ScreenShareConfig{
				SourceID:           "screen:0",
				Resolution:         domain.Resolution1080p,
				FPS:                60,
				IncludeSystemAudio: true,
			})
			if startErr == nil {
				okMu.Lock()
				okCount++
				okMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if okCount != 1 {
		t.Fatalf("concurrent StartScreenShare successes = %d, want 1", okCount)
	}

	state := svc.GetState()
	if !state.ScreenSharing || state.ScreenShareState != string(domain.ScreenShareStreaming) {
		t.Fatalf("unexpected screen share state after start: %+v", state)
	}

	// Push one RGBA frame into the stub capturer.
	evMu.Lock()
	capRef := lastCap
	evMu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	capRef.framesCh <- capture.FramePacket{Image: img, CapturedAt: time.Now()}

	// Simulate window close / Wayland permission revocation -> must auto-stop & emit screenshare:stopped (FR-05).
	capRef.ForceCloseWithErr(capture.ErrSourceClosed)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evMu.Lock()
		n := len(stoppedEv)
		evMu.Unlock()
		if n > 0 && !svc.GetState().ScreenSharing {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	evMu.Lock()
	nStopped := len(stoppedEv)
	evMu.Unlock()
	if nStopped == 0 {
		t.Fatalf("expected EventScreenShareStopped to be emitted on source close")
	}
	if svc.GetState().ScreenShareState != string(domain.ScreenShareIdle) {
		t.Fatalf("expected ScreenShareState == IDLE after auto-stop, got %q", svc.GetState().ScreenShareState)
	}
}

func TestVoiceServiceScreenShareHTTPFallbackRoutes(t *testing.T) {
	hubURL := startLocalTestHub(t)

	svc := NewVoiceService(ServiceConfig{
		HubAddr:         hubURL,
		Nickname:        "http-user",
		STUNServers:     []string{},
		IncludeLoopback: true,
	})
	svc.disableFFmpeg = true
	svc.sourcesFetcher = func(context.Context) ([]domain.ShareableSource, error) {
		return []domain.ShareableSource{
			{ID: "screen:0", Title: "Screen 0", Type: domain.SourceTypeScreen, Width: 1280, Height: 720},
		}, nil
	}
	svc.capturerFactory = func(cfg capture.CaptureConfig, pool *encoder.FramePool) (capture.Capturer, error) {
		return newStubCapturer(cfg.IncludeSystemAudio), nil
	}
	defer func() { _ = svc.Shutdown() }()

	if _, err := svc.JoinCall(); err != nil {
		t.Fatalf("JoinCall() failed: %v", err)
	}

	// GET /api/screenshare/sources
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/screenshare/sources", nil)
	svc.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/screenshare/sources status = %d", rec.Code)
	}
	var sources []domain.ShareableSource
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil || len(sources) != 1 {
		t.Fatalf("decode sources failed: err=%v len=%d", err, len(sources))
	}

	// POST /api/screenshare/start
	body, _ := json.Marshal(domain.ScreenShareConfig{
		SourceID:   "screen:0",
		Resolution: domain.Resolution720p,
		FPS:        30,
	})
	recStart := httptest.NewRecorder()
	svc.ServeHTTP(recStart, httptest.NewRequest(http.MethodPost, "/api/screenshare/start", bytes.NewReader(body)))
	if recStart.Code != http.StatusOK {
		t.Fatalf("POST /api/screenshare/start status = %d body=%s", recStart.Code, recStart.Body.String())
	}

	// POST /api/screenshare/stop
	recStop := httptest.NewRecorder()
	svc.ServeHTTP(recStop, httptest.NewRequest(http.MethodPost, "/api/screenshare/stop", nil))
	if recStop.Code != http.StatusOK {
		t.Fatalf("POST /api/screenshare/stop status = %d", recStop.Code)
	}
	if svc.GetState().ScreenSharing {
		t.Fatalf("expected ScreenSharing == false after /api/screenshare/stop")
	}
}
