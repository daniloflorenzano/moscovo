package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"

	"moscovo/internal/audio"
	"moscovo/internal/domain"
	"moscovo/internal/screenshare/capture"
	"moscovo/internal/screenshare/encoder"
	"moscovo/internal/transport"
)

const (
	EventHubState           = "hub:state"
	EventRoomState          = "room:roster"
	EventPeerStats          = "peer:stats"
	EventMicRaw             = "audio:mic-raw"
	EventScreenShareStopped = "screenshare:stopped"
	EventScreenShareState   = "screenshare:state"
	EventScreenShareFrame   = "screenshare:frame"

	defaultHubAddr = "127.0.0.1:7878"
)

// EventEmitter delivers backend state updates to the desktop UI.
type EventEmitter func(name string, data any)

// ServiceConfig configures VoiceService defaults and network hooks.
type ServiceConfig struct {
	HubAddr         string
	Nickname        string
	STUNServers     []string
	ForceRelay      bool
	IncludeLoopback bool
	Emit            EventEmitter
	Logger          *slog.Logger
}

// AppState is the single-screen state snapshot exposed to the Wails frontend.
type AppState struct {
	HubAddr            string                      `json:"hubAddr"`
	LocalID            string                      `json:"localId"`
	Nickname           string                      `json:"nickname"`
	HubStatus          string                      `json:"hubStatus"`
	MicDevice          string                      `json:"micDevice"`
	OutputDevice       string                      `json:"outputDevice"`
	InCall             bool                        `json:"inCall"`
	Muted              bool                        `json:"muted"`
	Deafened           bool                        `json:"deafened"`
	PushToTalk         bool                        `json:"pushToTalk"`
	Speaking           bool                        `json:"speaking"`
	NoiseSuppression   domain.NoiseSuppressionMode `json:"noiseSuppression"`
	ScreenShareState   string                      `json:"screenShareState"`
	ScreenSharing      bool                        `json:"screenSharing"`
	ScreenShareConfig  *domain.ScreenShareConfig   `json:"screenShareConfig,omitempty"`
	ActiveScreenPeerID string                      `json:"activeScreenPeerId,omitempty"`
	IsWayland          bool                        `json:"isWayland"`
	LastError          string                      `json:"lastError,omitempty"`
	Peers              []domain.PeerSession        `json:"peers"`
}

// VoiceService coordinates the Hub signaling client, WebRTC mesh, audio engine, and screen share for the UI.
type VoiceService struct {
	mu                 sync.Mutex
	hubAddr            string
	localID            string
	nickname           string
	hubConnState       transport.HubConnectionState
	micDevice          string
	outputDevice       string
	inCall             bool
	muted              bool
	deafened           bool
	pushToTalk         bool
	noiseSuppression   domain.NoiseSuppressionMode
	lastError          string
	peers              []domain.PeerSession
	stunServers        []string
	forceRelay         bool
	includeLoopback    bool
	emit               EventEmitter
	logger             *slog.Logger
	screenLifecycle    *domain.ScreenShareLifecycle
	screenConfig       *domain.ScreenShareConfig
	screenPool         *encoder.FramePool
	screenPipe         *capture.Pipeline
	activeScreenPeerID string
	lastSources        []domain.ShareableSource
	capturerFactory    func(capture.CaptureConfig, *encoder.FramePool) (capture.Capturer, error)
	sourcesFetcher     func(context.Context) ([]domain.ShareableSource, error)
	disableFFmpeg      bool

	callCancel context.CancelFunc
	callWG     *sync.WaitGroup
	hubClient  *transport.HubClient
	mesh       *transport.WebRTCMesh
	audioEng   *audio.Engine
}

// NewVoiceService constructs a VoiceService with sensible defaults.
func NewVoiceService(cfg ServiceConfig) *VoiceService {
	hubAddr := strings.TrimSpace(cfg.HubAddr)
	if hubAddr == "" {
		hubAddr = defaultHubAddr
	}
	nickname := strings.TrimSpace(cfg.Nickname)
	if nickname == "" {
		if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
			nickname = host
		} else {
			nickname = "moscovo-user"
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	micDev, outDev := audio.DetectAudioDevices()

	return &VoiceService{
		hubAddr:          hubAddr,
		localID:          generatePeerID(nickname),
		nickname:         nickname,
		hubConnState:     transport.HubOffline,
		micDevice:        micDev,
		outputDevice:     outDev,
		noiseSuppression: domain.DefaultNoiseSuppressionMode,
		stunServers:      cfg.STUNServers,
		forceRelay:       cfg.ForceRelay,
		includeLoopback:  cfg.IncludeLoopback,
		emit:             cfg.Emit,
		logger:           logger,
		peers:            []domain.PeerSession{},
		screenLifecycle:  domain.NewScreenShareLifecycle(),
		capturerFactory:  capture.NewPlatformCapturer,
		sourcesFetcher:   capture.GetShareableSources,
	}
}

// SetEmitter attaches or updates the event emitter callback (e.g., Wails App.Event.Emit).
func (s *VoiceService) SetEmitter(emit EventEmitter) {
	s.mu.Lock()
	s.emit = emit
	s.mu.Unlock()
}

// GetState returns the current application snapshot.
func (s *VoiceService) GetState() AppState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// SetHubConfig updates the Shared Hub address and user nickname, rejoining if currently in a call.
func (s *VoiceService) SetHubConfig(hubAddr, nickname string) (AppState, error) {
	trimmedAddr := strings.TrimSpace(hubAddr)
	trimmedNick := strings.TrimSpace(nickname)
	if _, err := transport.NormalizeHubWSURL(trimmedAddr); err != nil {
		s.setError(err.Error())
		return s.GetState(), err
	}
	if trimmedNick == "" {
		err := errors.New("nickname cannot be empty")
		s.setError(err.Error())
		return s.GetState(), err
	}

	s.mu.Lock()
	wasInCall := s.inCall
	s.hubAddr = trimmedAddr
	s.nickname = trimmedNick
	s.localID = generatePeerID(trimmedNick)
	s.lastError = ""
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventHubState, state)
	if wasInCall {
		if _, err := s.LeaveCall(); err != nil {
			return s.GetState(), err
		}
		return s.JoinCall()
	}
	return state, nil
}

// JoinCall connects to the Shared Hub and joins the voice mesh.
func (s *VoiceService) JoinCall() (AppState, error) {
	s.mu.Lock()
	if s.inCall {
		state := s.snapshotLocked()
		s.mu.Unlock()
		return state, nil
	}
	hubAddr := s.hubAddr
	localPeer := domain.PeerInfo{ID: s.localID, Name: s.nickname}
	stunServers := s.stunServers
	forceRelay := s.forceRelay
	includeLoopback := s.includeLoopback
	muted := s.muted
	deafened := s.deafened
	ptt := s.pushToTalk
	logger := s.logger
	s.mu.Unlock()

	micDev, outDev := audio.DetectAudioDevices()
	s.mu.Lock()
	s.micDevice = micDev
	s.outputDevice = outDev
	s.mu.Unlock()

	eng := audio.NewEngine(audio.EngineConfig{
		OnSpeakingChange: func(speaking bool) {
			s.handleLocalSpeakingChanged(speaking)
		},
		OnRawMicFrame: func(pcm []byte) {
			s.mu.Lock()
			active := s.inCall && s.noiseSuppression != domain.NoiseSuppressionOff
			s.mu.Unlock()
			if !active || len(pcm) == 0 {
				return
			}
			s.emitState(EventMicRaw, base64.StdEncoding.EncodeToString(pcm))
		},
	})
	eng.SetMuted(muted)
	eng.SetDeafened(deafened)
	eng.SetPushToTalk(ptt)

	var (
		clientMu sync.Mutex
		client   *transport.HubClient
		mesh     *transport.WebRTCMesh
	)

	c, err := transport.NewHubClient(transport.HubClientConfig{
		HubURL: hubAddr,
		Peer:   localPeer,
		Logger: logger,
		OnStateChange: func(connState transport.HubConnectionState, connErr error) {
			s.handleHubState(connState, connErr)
		},
		OnEnvelope: func(env domain.SignalEnvelope) {
			clientMu.Lock()
			m := mesh
			clientMu.Unlock()
			if m == nil {
				return
			}
			switch env.Type {
			case domain.MsgRoster:
				if applyErr := m.ApplyRoster(env.Peers, env.TURN); applyErr != nil {
					s.setError(applyErr.Error())
				}
			case domain.MsgOffer, domain.MsgAnswer, domain.MsgCandidate:
				if sigErr := m.HandleSignal(env); sigErr != nil {
					logger.Debug("signal handling error", "type", env.Type, "err", sigErr)
				}
			case domain.MsgError:
				s.setError(env.Error)
			}
		},
	})
	if err != nil {
		_ = eng.Close()
		s.setError(err.Error())
		return s.GetState(), err
	}

	m, err := transport.NewWebRTCMesh(transport.MeshConfig{
		LocalPeer:       localPeer,
		STUNServers:     stunServers,
		ForceRelay:      forceRelay,
		IncludeLoopback: includeLoopback,
		Audio:           eng,
		Logger:          logger,
		SendSignal: func(ctx context.Context, env domain.SignalEnvelope) error {
			clientMu.Lock()
			hc := client
			clientMu.Unlock()
			if hc == nil {
				return errors.New("hub client unavailable")
			}
			return hc.Send(ctx, env)
		},
		OnSessionsChange: func(sessions []domain.PeerSession) {
			s.handleSessionsChanged(sessions)
		},
		OnRemoteScreenFrame: func(peerID string, jpegData []byte, w, h int) {
			s.emitState(EventScreenShareFrame, map[string]any{
				"peerId": peerID,
				"jpeg":   base64.StdEncoding.EncodeToString(jpegData),
				"width":  w,
				"height": h,
			})
		},
		OnRemoteScreenState: func(peerID string, active bool, hasAudio bool) {
			s.handleRemoteScreenState(peerID, active, hasAudio)
		},
		OnScreenBitrateChange: func(rembBps uint64, lossFraction float64, rttMs int) {
			s.mu.Lock()
			pipe := s.screenPipe
			s.mu.Unlock()
			if pipe != nil {
				pipe.ApplyFeedback(rembBps, lossFraction, rttMs)
			}
		},
		OnScreenKeyframeRequest: func() {
			s.mu.Lock()
			pipe := s.screenPipe
			s.mu.Unlock()
			if pipe != nil {
				pipe.RequestKeyFrame()
			}
		},
	})
	if err != nil {
		_ = eng.Close()
		s.setError(err.Error())
		return s.GetState(), err
	}

	clientMu.Lock()
	client = c
	mesh = m
	clientMu.Unlock()

	callCtx, callCancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}

	s.mu.Lock()
	if s.inCall {
		state := s.snapshotLocked()
		s.mu.Unlock()
		callCancel()
		_ = m.Close()
		_ = eng.Close()
		return state, nil
	}
	s.inCall = true
	s.hubConnState = transport.HubConnecting
	s.lastError = ""
	s.peers = []domain.PeerSession{}
	s.callCancel = callCancel
	s.callWG = wg
	s.hubClient = c
	s.mesh = m
	s.audioEng = eng
	state := s.snapshotLocked()
	s.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.RunWithReconnect(callCtx)
	}()

	s.emitState(EventHubState, state)
	s.emitState(EventRoomState, state)
	return state, nil
}

// LeaveCall exits the voice room, closes PeerConnections, and stops audio and screen-share pipelines cleanly.
func (s *VoiceService) LeaveCall() (AppState, error) {
	_, _ = s.StopScreenShare()

	s.mu.Lock()
	if !s.inCall {
		state := s.snapshotLocked()
		s.mu.Unlock()
		return state, nil
	}
	cancel := s.callCancel
	wg := s.callWG
	client := s.hubClient
	mesh := s.mesh
	eng := s.audioEng

	s.inCall = false
	s.hubConnState = transport.HubOffline
	s.peers = []domain.PeerSession{}
	s.activeScreenPeerID = ""
	s.callCancel = nil
	s.callWG = nil
	s.hubClient = nil
	s.mesh = nil
	s.audioEng = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.LeaveAndClose()
	}
	if mesh != nil {
		_ = mesh.Close()
	}
	if eng != nil {
		_ = eng.Close()
	}
	if wg != nil {
		wg.Wait()
	}

	state := s.GetState()
	s.emitState(EventHubState, state)
	s.emitState(EventRoomState, state)
	return state, nil
}

// ToggleCall joins if offline or leaves if already in a call.
func (s *VoiceService) ToggleCall() (AppState, error) {
	s.mu.Lock()
	inCall := s.inCall
	s.mu.Unlock()
	if inCall {
		return s.LeaveCall()
	}
	return s.JoinCall()
}

// ToggleMute toggles microphone mute state and broadcasts control telemetry.
func (s *VoiceService) ToggleMute() (AppState, error) {
	s.mu.Lock()
	s.muted = !s.muted
	s.applyAudioControlLocked()
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventHubState, state)
	return state, nil
}

// SetPushToTalk activates or releases push-to-talk override while muted.
func (s *VoiceService) SetPushToTalk(active bool) (AppState, error) {
	s.mu.Lock()
	if s.pushToTalk == active {
		state := s.snapshotLocked()
		s.mu.Unlock()
		return state, nil
	}
	s.pushToTalk = active
	s.applyAudioControlLocked()
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventHubState, state)
	return state, nil
}

// ToggleDeafen toggles output deafen (suppressing both playback and mic).
func (s *VoiceService) ToggleDeafen() (AppState, error) {
	s.mu.Lock()
	s.deafened = !s.deafened
	s.applyAudioControlLocked()
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventHubState, state)
	return state, nil
}

// CycleNoiseSuppression advances through off -> rnnoise -> speex -> gate -> off.
func (s *VoiceService) CycleNoiseSuppression() (AppState, error) {
	s.mu.Lock()
	s.noiseSuppression = domain.NextNoiseSuppressionMode(s.noiseSuppression)
	eng := s.audioEng
	off := s.noiseSuppression == domain.NoiseSuppressionOff
	state := s.snapshotLocked()
	s.mu.Unlock()

	if off && eng != nil {
		eng.ClearProcessedMicFrame()
	}
	s.emitState(EventHubState, state)
	return state, nil
}

// SetNoiseSuppression selects a specific client-side noise suppression mode.
func (s *VoiceService) SetNoiseSuppression(mode string) (AppState, error) {
	parsed := domain.NoiseSuppressionMode(strings.ToLower(strings.TrimSpace(mode)))
	if !domain.IsValidNoiseSuppressionMode(parsed) {
		err := fmt.Errorf("invalid noise suppression mode %q", mode)
		s.setError(err.Error())
		return s.GetState(), err
	}

	s.mu.Lock()
	s.noiseSuppression = parsed
	eng := s.audioEng
	off := parsed == domain.NoiseSuppressionOff
	state := s.snapshotLocked()
	s.mu.Unlock()

	if off && eng != nil {
		eng.ClearProcessedMicFrame()
	}
	s.emitState(EventHubState, state)
	return state, nil
}

// SubmitProcessedMicFrame accepts a base64-encoded 16kHz PCM16 frame denoised by the frontend AudioWorklet.
func (s *VoiceService) SubmitProcessedMicFrame(pcmBase64 string) error {
	if pcmBase64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(pcmBase64)
	if err != nil {
		return err
	}
	s.mu.Lock()
	active := s.inCall && s.noiseSuppression != domain.NoiseSuppressionOff
	eng := s.audioEng
	s.mu.Unlock()
	if !active || eng == nil {
		return nil
	}
	eng.PushProcessedMicFrame(raw)
	return nil
}

// SetPeerVolume adjusts a remote peer's local playback percentage (0..200).
func (s *VoiceService) SetPeerVolume(peerID string, volume int) (AppState, error) {
	s.mu.Lock()
	mesh := s.mesh
	s.mu.Unlock()
	if mesh == nil {
		return s.GetState(), errors.New("not in a call")
	}
	if err := mesh.SetPeerVolume(peerID, volume); err != nil {
		s.setError(err.Error())
		return s.GetState(), err
	}
	return s.GetState(), nil
}

// TogglePeerMute mutes or unmutes a specific remote peer locally.
func (s *VoiceService) TogglePeerMute(peerID string) (AppState, error) {
	s.mu.Lock()
	mesh := s.mesh
	s.mu.Unlock()
	if mesh == nil {
		return s.GetState(), errors.New("not in a call")
	}
	if _, err := mesh.TogglePeerMute(peerID); err != nil {
		s.setError(err.Error())
		return s.GetState(), err
	}
	return s.GetState(), nil
}

// GetShareableSources enumerates monitors, windows, or the Wayland ScreenCast portal (FR-01, FR-02).
func (s *VoiceService) GetShareableSources() ([]domain.ShareableSource, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.mu.Lock()
	fetcher := s.sourcesFetcher
	s.mu.Unlock()
	if fetcher == nil {
		fetcher = capture.GetShareableSources
	}

	sources, err := fetcher(ctx)
	if err != nil {
		s.setError(err.Error())
		return nil, err
	}

	s.mu.Lock()
	s.lastSources = append([]domain.ShareableSource(nil), sources...)
	s.mu.Unlock()
	return sources, nil
}

// StartScreenShare initializes capture, encoding, and dynamic WebRTC screen-share tracks (FR-02..FR-06, NFR-06).
func (s *VoiceService) StartScreenShare(cfg domain.ScreenShareConfig) (AppState, error) {
	norm, err := cfg.Normalized()
	if err != nil {
		s.setError(err.Error())
		return s.GetState(), err
	}

	if err := s.screenLifecycle.TryStart(); err != nil {
		return s.GetState(), err
	}

	s.mu.Lock()
	inCall := s.inCall
	mesh := s.mesh
	sources := append([]domain.ShareableSource(nil), s.lastSources...)
	capFactory := s.capturerFactory
	disableFFmpeg := s.disableFFmpeg
	s.mu.Unlock()

	if !inCall || mesh == nil {
		s.screenLifecycle.ResetIdle()
		err := errors.New("join a voice call before sharing screen")
		s.setError(err.Error())
		return s.GetState(), err
	}
	if capFactory == nil {
		capFactory = capture.NewPlatformCapturer
	}

	srcW, srcH := 1920, 1080
	for _, src := range sources {
		if src.ID == norm.SourceID && src.Width > 0 && src.Height > 0 {
			srcW, srcH = src.Width, src.Height
			break
		}
	}
	dstW, dstH := domain.ResolveTargetDimensions(srcW, srcH, norm.Resolution)

	pool := encoder.NewFramePool()
	vidEnc, err := encoder.NewVideoEncoder(encoder.EncoderConfig{
		Width:          dstW,
		Height:         dstH,
		FPS:            norm.FPS,
		BitrateBps:     domain.DefaultVideoBitrateBps,
		PreferHardware: true,
		DisableFFmpeg:  disableFFmpeg,
	})
	if err != nil {
		s.screenLifecycle.ResetIdle()
		s.setError(err.Error())
		return s.GetState(), err
	}

	var audEnc *encoder.SystemAudioEncoder
	if norm.IncludeSystemAudio {
		audEnc = encoder.NewSystemAudioEncoder(encoder.SystemAudioCaptureRate, encoder.SystemAudioCaptureChannels)
	}

	capturer, err := capFactory(capture.CaptureConfig{
		SourceID:           norm.SourceID,
		Resolution:         norm.Resolution,
		FPS:                norm.FPS,
		IncludeSystemAudio: norm.IncludeSystemAudio,
		TargetWidth:        dstW,
		TargetHeight:       dstH,
	}, pool)
	if err != nil {
		_ = vidEnc.Close()
		if audEnc != nil {
			_ = audEnc.Close()
		}
		s.screenLifecycle.ResetIdle()
		s.setError(err.Error())
		return s.GetState(), err
	}

	pipe, err := capture.NewPipeline(capture.PipelineConfig{
		Capturer:     capturer,
		Encoder:      vidEnc,
		AudioEncoder: audEnc,
		Pool:         pool,
		TargetWidth:  dstW,
		TargetHeight: dstH,
		OnVideoSample: func(f encoder.EncodedFrame) {
			_ = mesh.WriteScreenVideoSample(media.Sample{
				Data:     f.Data,
				Duration: f.Duration,
			})
		},
		OnAudioSample: func(data []byte, dur time.Duration) {
			_ = mesh.WriteScreenAudioSample(media.Sample{
				Data:     data,
				Duration: dur,
			})
		},
		OnStopped: func(reason error) {
			s.stopScreenShareInternal(reason)
		},
	})
	if err != nil {
		_ = capturer.Close()
		_ = vidEnc.Close()
		if audEnc != nil {
			_ = audEnc.Close()
		}
		s.screenLifecycle.ResetIdle()
		s.setError(err.Error())
		return s.GetState(), err
	}

	if err := mesh.StartScreenShareTracks(vidEnc.CodecMime(), norm.IncludeSystemAudio); err != nil {
		_ = pipe.Stop()
		s.screenLifecycle.ResetIdle()
		s.setError(err.Error())
		return s.GetState(), err
	}

	if err := pipe.Start(context.Background()); err != nil {
		_ = pipe.Stop()
		_ = mesh.StopScreenShareTracks()
		s.screenLifecycle.ResetIdle()
		s.setError(err.Error())
		return s.GetState(), err
	}

	if err := s.screenLifecycle.MarkStreaming(); err != nil {
		// Concurrent stop arrived while starting.
		_ = pipe.Stop()
		_ = mesh.StopScreenShareTracks()
		s.screenLifecycle.ResetIdle()
		return s.GetState(), err
	}

	s.mu.Lock()
	s.screenPool = pool
	s.screenPipe = pipe
	cfgCopy := norm
	s.screenConfig = &cfgCopy
	s.lastError = ""
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventScreenShareState, state)
	s.emitState(EventHubState, state)
	return state, nil
}

// StopScreenShare terminates local screen sharing, removes WebRTC tracks, and emits screenshare:stopped (FR-05).
func (s *VoiceService) StopScreenShare() (AppState, error) {
	s.stopScreenShareInternal(nil)
	return s.GetState(), nil
}

func (s *VoiceService) stopScreenShareInternal(reason error) {
	if err := s.screenLifecycle.BeginStop(); err != nil {
		return
	}

	s.mu.Lock()
	pipe := s.screenPipe
	mesh := s.mesh
	s.screenPipe = nil
	s.screenPool = nil
	s.screenConfig = nil
	s.mu.Unlock()

	if pipe != nil {
		_ = pipe.Stop()
	}
	if mesh != nil {
		_ = mesh.StopScreenShareTracks()
	}
	s.screenLifecycle.ResetIdle()

	reasonText := ""
	if reason != nil {
		reasonText = reason.Error()
	}
	state := s.GetState()
	s.emitState(EventScreenShareStopped, map[string]any{
		"reason": reasonText,
	})
	s.emitState(EventScreenShareState, state)
	s.emitState(EventHubState, state)
}

// NegotiateScreenViewer exchanges SDP with the frontend RTCPeerConnection for remote video ontrack delivery.
func (s *VoiceService) NegotiateScreenViewer(offerSDP string) (string, error) {
	s.mu.Lock()
	mesh := s.mesh
	s.mu.Unlock()
	if mesh == nil {
		return "", errors.New("not in a call")
	}
	return mesh.NegotiateViewerSDP(offerSDP)
}

// Shutdown cleanly exits any active call before application termination.
func (s *VoiceService) Shutdown() error {
	_, err := s.LeaveCall()
	return err
}

// ServeHTTP exposes REST fallback endpoints when mounted at /api in Wails.
func (s *VoiceService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	if path == "" {
		path = "/"
	}

	switch {
	case r.Method == http.MethodGet && path == "/state":
		writeJSON(w, http.StatusOK, s.GetState())

	case r.Method == http.MethodPost && path == "/config":
		var req struct {
			HubAddr  string `json:"hubAddr"`
			Nickname string `json:"nickname"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, err := s.SetHubConfig(req.HubAddr, req.Nickname)
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/call/join":
		state, err := s.JoinCall()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/call/leave":
		state, err := s.LeaveCall()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/call/toggle":
		state, err := s.ToggleCall()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/mic/mute":
		state, err := s.ToggleMute()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/mic/deafen":
		state, err := s.ToggleDeafen()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/mic/ptt":
		var req struct {
			Active bool `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, err := s.SetPushToTalk(req.Active)
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/mic/ns":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" {
			var req struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.Mode != "" {
				state, setErr := s.SetNoiseSuppression(req.Mode)
				writeStateResponse(w, state, setErr)
				return
			}
		}
		state, cycleErr := s.CycleNoiseSuppression()
		writeStateResponse(w, state, cycleErr)

	case r.Method == http.MethodPost && path == "/mic/frame":
		var req struct {
			PCM string `json:"pcm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.SubmitProcessedMicFrame(req.PCM); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case r.Method == http.MethodPost && path == "/peer/volume":
		var req struct {
			PeerID string `json:"peerId"`
			Volume int    `json:"volume"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, err := s.SetPeerVolume(req.PeerID, req.Volume)
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/peer/mute":
		var req struct {
			PeerID string `json:"peerId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, err := s.TogglePeerMute(req.PeerID)
		writeStateResponse(w, state, err)

	case r.Method == http.MethodGet && path == "/screenshare/sources":
		sources, err := s.GetShareableSources()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sources)

	case r.Method == http.MethodPost && path == "/screenshare/start":
		var req domain.ScreenShareConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, err := s.StartScreenShare(req)
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/screenshare/stop":
		state, err := s.StopScreenShare()
		writeStateResponse(w, state, err)

	case r.Method == http.MethodPost && path == "/screenshare/viewer":
		var req struct {
			SDP string `json:"sdp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		answerSDP, err := s.NegotiateScreenViewer(req.SDP)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"sdp": answerSDP})

	default:
		http.NotFound(w, r)
	}
}

func (s *VoiceService) applyAudioControlLocked() {
	if s.audioEng != nil {
		s.audioEng.SetMuted(s.muted)
		s.audioEng.SetDeafened(s.deafened)
		s.audioEng.SetPushToTalk(s.pushToTalk)
	}
	if s.mesh != nil {
		effMuted := s.deafened || (s.muted && !s.pushToTalk)
		speaking := false
		if s.audioEng != nil {
			speaking = s.audioEng.Speaking()
		}
		s.mesh.BroadcastControlState(effMuted, speaking)
	}
}

func (s *VoiceService) handleHubState(connState transport.HubConnectionState, err error) {
	if err != nil {
		s.logger.Warn("hub state changed", "state", connState, "err", err)
	} else {
		s.logger.Info("hub state changed", "state", connState)
	}
	s.mu.Lock()
	if !s.inCall && connState != transport.HubOffline {
		s.mu.Unlock()
		return
	}
	s.hubConnState = connState
	if err != nil {
		s.lastError = err.Error()
	} else if connState == transport.HubOnline {
		s.lastError = ""
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventHubState, state)
}

func (s *VoiceService) handleSessionsChanged(sessions []domain.PeerSession) {
	s.mu.Lock()
	if !s.inCall {
		s.mu.Unlock()
		return
	}
	s.peers = append([]domain.PeerSession(nil), sessions...)
	s.reconcileActiveScreenPeerLocked()
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventRoomState, state)
	s.emitState(EventPeerStats, state.Peers)
}

func (s *VoiceService) handleRemoteScreenState(peerID string, active bool, _ bool) {
	s.mu.Lock()
	if !s.inCall {
		s.mu.Unlock()
		return
	}
	if active {
		s.activeScreenPeerID = peerID
	} else if s.activeScreenPeerID == peerID {
		s.activeScreenPeerID = ""
		s.reconcileActiveScreenPeerLocked()
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventScreenShareState, state)
	s.emitState(EventHubState, state)
}

func (s *VoiceService) reconcileActiveScreenPeerLocked() {
	if s.activeScreenPeerID != "" {
		for _, p := range s.peers {
			if p.ID == s.activeScreenPeerID && p.ScreenSharing {
				return
			}
		}
		s.activeScreenPeerID = ""
	}
	for _, p := range s.peers {
		if p.ScreenSharing {
			s.activeScreenPeerID = p.ID
			return
		}
	}
}

func (s *VoiceService) setError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	state := s.snapshotLocked()
	s.mu.Unlock()
	s.emitState(EventHubState, state)
}

func (s *VoiceService) handleLocalSpeakingChanged(speaking bool) {
	s.mu.Lock()
	if !s.inCall {
		s.mu.Unlock()
		return
	}
	mesh := s.mesh
	effMuted := s.deafened || (s.muted && !s.pushToTalk)
	state := s.snapshotLocked()
	state.Speaking = speaking && !effMuted
	s.mu.Unlock()

	if mesh != nil {
		mesh.BroadcastControlState(effMuted, state.Speaking)
	}
	s.emitState(EventHubState, state)
}

func (s *VoiceService) snapshotLocked() AppState {
	status := string(s.hubConnState)
	if s.inCall && s.hubConnState == transport.HubOnline {
		status = "IN CALL"
	}
	speaking := false
	if s.audioEng != nil {
		speaking = s.audioEng.Speaking()
	}
	peersCopy := append([]domain.PeerSession(nil), s.peers...)
	screenState := s.screenLifecycle.State()
	var cfgCopy *domain.ScreenShareConfig
	if s.screenConfig != nil {
		c := *s.screenConfig
		cfgCopy = &c
	}
	return AppState{
		HubAddr:            s.hubAddr,
		LocalID:            s.localID,
		Nickname:           s.nickname,
		HubStatus:          status,
		MicDevice:          s.micDevice,
		OutputDevice:       s.outputDevice,
		InCall:             s.inCall,
		Muted:              s.muted,
		Deafened:           s.deafened,
		PushToTalk:         s.pushToTalk,
		Speaking:           speaking,
		NoiseSuppression:   s.noiseSuppression,
		ScreenShareState:   string(screenState),
		ScreenSharing:      screenState == domain.ScreenShareStreaming,
		ScreenShareConfig:  cfgCopy,
		ActiveScreenPeerID: s.activeScreenPeerID,
		IsWayland:          capture.IsWaylandSession(),
		LastError:          s.lastError,
		Peers:              peersCopy,
	}
}

func (s *VoiceService) emitState(name string, payload any) {
	s.mu.Lock()
	emit := s.emit
	s.mu.Unlock()
	if emit != nil {
		emit(name, payload)
	}
}

func generatePeerID(nickname string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	slug := strings.ToLower(strings.TrimSpace(nickname))
	var cleaned strings.Builder
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	prefix := cleaned.String()
	if prefix == "" {
		prefix = "peer"
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b[:]))
}

func writeStateResponse(w http.ResponseWriter, state AppState, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
			"state": state,
		})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
