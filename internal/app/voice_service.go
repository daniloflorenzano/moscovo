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

	"moscovo/internal/audio"
	"moscovo/internal/domain"
	"moscovo/internal/transport"
)

const (
	EventHubState  = "hub:state"
	EventRoomState = "room:roster"
	EventPeerStats = "peer:stats"
	EventMicRaw    = "audio:mic-raw"

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
	HubAddr          string                      `json:"hubAddr"`
	LocalID          string                      `json:"localId"`
	Nickname         string                      `json:"nickname"`
	HubStatus        string                      `json:"hubStatus"`
	MicDevice        string                      `json:"micDevice"`
	OutputDevice     string                      `json:"outputDevice"`
	InCall           bool                        `json:"inCall"`
	Muted            bool                        `json:"muted"`
	Deafened         bool                        `json:"deafened"`
	PushToTalk       bool                        `json:"pushToTalk"`
	Speaking         bool                        `json:"speaking"`
	NoiseSuppression domain.NoiseSuppressionMode `json:"noiseSuppression"`
	LastError        string                      `json:"lastError,omitempty"`
	Peers            []domain.PeerSession        `json:"peers"`
}

// VoiceService coordinates the Hub signaling client, WebRTC mesh, and audio engine for the UI.
type VoiceService struct {
	mu               sync.Mutex
	hubAddr          string
	localID          string
	nickname         string
	hubConnState     transport.HubConnectionState
	micDevice        string
	outputDevice     string
	inCall           bool
	muted            bool
	deafened         bool
	pushToTalk       bool
	noiseSuppression domain.NoiseSuppressionMode
	lastError        string
	peers            []domain.PeerSession
	stunServers      []string
	forceRelay       bool
	includeLoopback  bool
	emit             EventEmitter
	logger           *slog.Logger

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

// LeaveCall exits the voice room, closes PeerConnections, and stops audio pipelines cleanly.
func (s *VoiceService) LeaveCall() (AppState, error) {
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
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.emitState(EventRoomState, state)
	s.emitState(EventPeerStats, state.Peers)
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
	return AppState{
		HubAddr:          s.hubAddr,
		LocalID:          s.localID,
		Nickname:         s.nickname,
		HubStatus:        status,
		MicDevice:        s.micDevice,
		OutputDevice:     s.outputDevice,
		InCall:           s.inCall,
		Muted:            s.muted,
		Deafened:         s.deafened,
		PushToTalk:       s.pushToTalk,
		Speaking:         speaking,
		NoiseSuppression: s.noiseSuppression,
		LastError:        s.lastError,
		Peers:            peersCopy,
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
