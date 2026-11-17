package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"moscovo/internal/audio"
	"moscovo/internal/domain"
)

const (
	defaultControlPingInterval = 1 * time.Second
	signalSendTimeout          = 3 * time.Second
)

var defaultSTUNServers = []string{"stun:stun.l.google.com:19302"}

// MeshConfig configures the client-side full-mesh Pion WebRTC engine.
type MeshConfig struct {
	LocalPeer        domain.PeerInfo
	STUNServers      []string
	ForceRelay       bool
	IncludeLoopback  bool
	PingInterval     time.Duration
	Audio            audio.AudioIO
	SendSignal       func(context.Context, domain.SignalEnvelope) error
	OnSessionsChange func([]domain.PeerSession)
	Logger           *slog.Logger
}

type controlMessage struct {
	Kind     string `json:"kind"`
	SentAtNs int64  `json:"sentAtNs,omitempty"`
	Muted    bool   `json:"muted,omitempty"`
	Speaking bool   `json:"speaking,omitempty"`
}

type peerConnEntry struct {
	mu                 sync.Mutex
	pc                 *webrtc.PeerConnection
	dc                 *webrtc.DataChannel
	session            domain.PeerSession
	initiate           bool
	remoteDescSet      bool
	pendingCandidates  []webrtc.ICECandidateInit
	dcSpeaking         bool
	audioSpeakingUntil time.Time
	closed             bool
	done               chan struct{}
	closeOnce          sync.Once
}

func (e *peerConnEntry) closeResources() {
	e.closeOnce.Do(func() {
		close(e.done)
		e.mu.Lock()
		e.closed = true
		dc := e.dc
		pc := e.pc
		_ = e.session.TransitionTo(domain.StateClosed)
		e.mu.Unlock()

		if dc != nil {
			_ = dc.Close()
		}
		if pc != nil {
			_ = pc.Close()
		}
	})
}

// WebRTCMesh manages per-peer WebRTC connections, Opus tracks, control DataChannels, and path stats.
type WebRTCMesh struct {
	localPeer        domain.PeerInfo
	stunServers      []string
	forceRelay       bool
	includeLoopback  bool
	pingInterval     time.Duration
	audioIO          audio.AudioIO
	sendSignal       func(context.Context, domain.SignalEnvelope) error
	onSessionsChange func([]domain.PeerSession)
	logger           *slog.Logger

	api        *webrtc.API
	localTrack *webrtc.TrackLocalStaticSample

	mu         sync.Mutex
	peers      map[string]*peerConnEntry
	turnCreds  *domain.TURNCredentials
	localMuted bool
	speaking   bool
	closed     bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWebRTCMesh initializes the WebRTC API, local Opus track, and microphone pump.
func NewWebRTCMesh(cfg MeshConfig) (*WebRTCMesh, error) {
	if strings.TrimSpace(cfg.LocalPeer.ID) == "" || strings.TrimSpace(cfg.LocalPeer.Name) == "" {
		return nil, errors.New("local peer id and name cannot be empty")
	}
	if cfg.Audio == nil {
		return nil, errors.New("audio engine cannot be nil")
	}
	if cfg.SendSignal == nil {
		return nil, errors.New("send signal callback cannot be nil")
	}

	stunServers := cfg.STUNServers
	if stunServers == nil {
		stunServers = append([]string(nil), defaultSTUNServers...)
	}
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = defaultControlPingInterval
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	se := webrtc.SettingEngine{}
	se.LoggerFactory = logging.NewDefaultLoggerFactory()
	se.SetInterfaceFilter(shouldKeepNetworkInterface)
	if cfg.IncludeLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	se.SetICETimeouts(5*time.Second, 15*time.Second, 1*time.Second)

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register default codecs: %w", err)
	}
	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(se),
		webrtc.WithMediaEngine(me),
	)

	localTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio",
		cfg.LocalPeer.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("create local opus track: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &WebRTCMesh{
		localPeer:        cfg.LocalPeer,
		stunServers:      stunServers,
		forceRelay:       cfg.ForceRelay,
		includeLoopback:  cfg.IncludeLoopback,
		pingInterval:     pingInterval,
		audioIO:          cfg.Audio,
		sendSignal:       cfg.SendSignal,
		onSessionsChange: cfg.OnSessionsChange,
		logger:           logger,
		api:              api,
		localTrack:       localTrack,
		peers:            make(map[string]*peerConnEntry),
		ctx:              ctx,
		cancel:           cancel,
	}

	if err := m.audioIO.Start(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("start audio io: %w", err)
	}

	m.wg.Add(1)
	go m.micPump()
	return m, nil
}

func shouldKeepNetworkInterface(name string) bool {
	ignoredPrefixes := []string{"docker", "veth", "br-", "virbr", "cni", "flannel"}
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func (m *WebRTCMesh) micPump() {
	defer m.wg.Done()
	for {
		sample, err := m.audioIO.ReadMicSample(m.ctx)
		if err != nil {
			return
		}
		if err := m.localTrack.WriteSample(media.Sample{
			Data:     sample.Data,
			Duration: sample.Duration,
		}); err != nil && !errors.Is(err, webrtc.ErrConnectionClosed) {
			m.logger.Debug("write local audio sample", "err", err)
		}
	}
}

// ApplyRoster reconciles active PeerConnections against the latest Hub roster.
func (m *WebRTCMesh) ApplyRoster(roster []domain.PeerInfo, turnCreds *domain.TURNCredentials) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("webrtc mesh closed")
	}
	if turnCreds != nil {
		copied := *turnCreds
		copied.URLs = append([]string(nil), turnCreds.URLs...)
		m.turnCreds = &copied
	}

	currentIDs := make([]string, 0, len(m.peers))
	for id, entry := range m.peers {
		currentIDs = append(currentIDs, id)
		for _, r := range roster {
			if r.ID == id {
				entry.mu.Lock()
				entry.session.Name = r.Name
				entry.mu.Unlock()
				break
			}
		}
	}

	diff, err := domain.ComputeMeshDiff(m.localPeer.ID, currentIDs, roster)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	var toClose []*peerConnEntry
	for _, id := range diff.ToDisconnect {
		if entry, ok := m.peers[id]; ok {
			toClose = append(toClose, entry)
			delete(m.peers, id)
		}
	}

	var toOffer []*peerConnEntry
	for _, target := range diff.ToConnect {
		entry, spawnErr := m.createPeerEntryLocked(target.Peer, target.InitiateOffer)
		if spawnErr != nil {
			m.mu.Unlock()
			for _, e := range toClose {
				e.closeResources()
			}
			return spawnErr
		}
		m.peers[target.Peer.ID] = entry
		if target.InitiateOffer {
			toOffer = append(toOffer, entry)
		}
	}
	m.mu.Unlock()

	for _, e := range toClose {
		e.closeResources()
	}
	for _, e := range toOffer {
		if err := m.sendOffer(e); err != nil {
			m.logger.Warn("failed to send initial offer", "peerId", e.session.ID, "err", err)
		}
	}

	m.notifySessionsChanged()
	return nil
}

// HandleSignal routes incoming offer, answer, or candidate frames to the right PeerConnection.
func (m *WebRTCMesh) HandleSignal(env domain.SignalEnvelope) error {
	switch env.Type {
	case domain.MsgOffer:
		return m.handleOffer(env)
	case domain.MsgAnswer:
		return m.handleAnswer(env)
	case domain.MsgCandidate:
		return m.handleCandidate(env)
	default:
		return nil
	}
}

func (m *WebRTCMesh) handleOffer(env domain.SignalEnvelope) error {
	entry, err := m.getOrCreatePoliteEntry(env.SenderID)
	if err != nil {
		return err
	}

	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return nil
	}
	pc := entry.pc
	entry.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  env.SDP,
	}); err != nil {
		return fmt.Errorf("set remote offer from %s: %w", env.SenderID, err)
	}

	m.flushPendingCandidates(entry)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer for %s: %w", env.SenderID, err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local answer for %s: %w", env.SenderID, err)
	}

	return m.emitSignal(domain.SignalEnvelope{
		Type:     domain.MsgAnswer,
		SenderID: m.localPeer.ID,
		TargetID: env.SenderID,
		SDP:      answer.SDP,
	})
}

func (m *WebRTCMesh) handleAnswer(env domain.SignalEnvelope) error {
	m.mu.Lock()
	entry, ok := m.peers[env.SenderID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("answer from unknown peer %q", env.SenderID)
	}

	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return nil
	}
	pc := entry.pc
	entry.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  env.SDP,
	}); err != nil {
		return fmt.Errorf("set remote answer from %s: %w", env.SenderID, err)
	}
	m.flushPendingCandidates(entry)
	return nil
}

func (m *WebRTCMesh) handleCandidate(env domain.SignalEnvelope) error {
	var candInit webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(env.Candidate), &candInit); err != nil {
		return fmt.Errorf("decode ice candidate from %s: %w", env.SenderID, err)
	}
	if !m.forceRelay && domain.ShouldFilterTailscaleHostCandidate(candInit.Candidate) {
		return nil
	}

	m.mu.Lock()
	entry, ok := m.peers[env.SenderID]
	m.mu.Unlock()
	if !ok {
		initiate, err := domain.ShouldInitiateOffer(m.localPeer.ID, env.SenderID)
		if err != nil || initiate {
			return nil
		}
		entry, err = m.getOrCreatePoliteEntry(env.SenderID)
		if err != nil {
			return err
		}
	}

	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return nil
	}
	if !entry.remoteDescSet {
		entry.pendingCandidates = append(entry.pendingCandidates, candInit)
		entry.mu.Unlock()
		return nil
	}
	pc := entry.pc
	entry.mu.Unlock()

	return pc.AddICECandidate(candInit)
}

func (m *WebRTCMesh) getOrCreatePoliteEntry(remoteID string) (*peerConnEntry, error) {
	initiate, err := domain.ShouldInitiateOffer(m.localPeer.ID, remoteID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("webrtc mesh closed")
	}
	if existing, ok := m.peers[remoteID]; ok {
		return existing, nil
	}
	if initiate {
		return nil, fmt.Errorf("unexpected offer from higher-id peer %q", remoteID)
	}
	entry, err := m.createPeerEntryLocked(domain.PeerInfo{ID: remoteID, Name: remoteID}, false)
	if err != nil {
		return nil, err
	}
	m.peers[remoteID] = entry
	return entry, nil
}

func (m *WebRTCMesh) flushPendingCandidates(entry *peerConnEntry) {
	entry.mu.Lock()
	entry.remoteDescSet = true
	pending := append([]webrtc.ICECandidateInit(nil), entry.pendingCandidates...)
	entry.pendingCandidates = nil
	pc := entry.pc
	closed := entry.closed
	entry.mu.Unlock()

	if closed {
		return
	}
	for _, cand := range pending {
		if err := pc.AddICECandidate(cand); err != nil {
			m.logger.Debug("add buffered ice candidate failed", "peerId", entry.session.ID, "err", err)
		}
	}
}

func (m *WebRTCMesh) createPeerEntryLocked(remote domain.PeerInfo, initiate bool) (*peerConnEntry, error) {
	session, err := domain.NewPeerSession(remote.ID, remote.Name)
	if err != nil {
		return nil, err
	}
	if err := session.TransitionTo(domain.StateSignaling); err != nil {
		return nil, err
	}

	iceServers := make([]webrtc.ICEServer, 0, 2)
	if !m.forceRelay && len(m.stunServers) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: m.stunServers})
	}
	if m.turnCreds != nil && len(m.turnCreds.URLs) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           append([]string(nil), m.turnCreds.URLs...),
			Username:       m.turnCreds.Username,
			Credential:     m.turnCreds.Password,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	policy := webrtc.ICETransportPolicyAll
	if m.forceRelay {
		policy = webrtc.ICETransportPolicyRelay
	}

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: policy,
	})
	if err != nil {
		return nil, fmt.Errorf("create peer connection for %s: %w", remote.ID, err)
	}

	if _, err := pc.AddTrack(m.localTrack); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add local audio track for %s: %w", remote.ID, err)
	}

	entry := &peerConnEntry{
		pc:       pc,
		session:  session,
		initiate: initiate,
		done:     make(chan struct{}),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || m.ctx.Err() != nil {
			return
		}
		candInit := c.ToJSON()
		if !m.forceRelay && domain.ShouldFilterTailscaleHostCandidate(candInit.Candidate) {
			return
		}
		raw, marshalErr := json.Marshal(candInit)
		if marshalErr != nil {
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			_ = m.emitSignal(domain.SignalEnvelope{
				Type:      domain.MsgCandidate,
				SenderID:  m.localPeer.ID,
				TargetID:  remote.ID,
				Candidate: string(raw),
			})
		}()
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		m.wg.Add(1)
		go m.readRemoteTrack(entry, track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.handlePCStateChange(entry, state)
	})

	if iceTransport := pc.SCTP().Transport().ICETransport(); iceTransport != nil {
		iceTransport.OnSelectedCandidatePairChange(func(pair *webrtc.ICECandidatePair) {
			m.updateSelectedPair(entry, pair)
		})
	}

	if initiate {
		dc, dcErr := pc.CreateDataChannel("control", nil)
		if dcErr != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("create control datachannel for %s: %w", remote.ID, dcErr)
		}
		m.attachDataChannel(entry, dc)
	} else {
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			if dc.Label() == "control" {
				m.attachDataChannel(entry, dc)
			}
		})
	}

	return entry, nil
}

func (m *WebRTCMesh) sendOffer(entry *peerConnEntry) error {
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return nil
	}
	pc := entry.pc
	targetID := entry.session.ID
	entry.mu.Unlock()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer for %s: %w", targetID, err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local offer for %s: %w", targetID, err)
	}

	return m.emitSignal(domain.SignalEnvelope{
		Type:     domain.MsgOffer,
		SenderID: m.localPeer.ID,
		TargetID: targetID,
		SDP:      offer.SDP,
	})
}

func (m *WebRTCMesh) readRemoteTrack(entry *peerConnEntry, track *webrtc.TrackRemote) {
	defer m.wg.Done()
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		energy := 0.0
		if len(pkt.Payload) >= 64 {
			energy = audio.ComputePCM16RMS(pkt.Payload)
		}

		entry.mu.Lock()
		closed := entry.closed
		peerID := entry.session.ID
		volume := entry.session.Volume
		localMuted := entry.session.LocalMuted
		if energy >= 0.02 {
			entry.audioSpeakingUntil = now.Add(250 * time.Millisecond)
		}
		nextSpeaking := !entry.session.RemoteMuted && (entry.dcSpeaking || now.Before(entry.audioSpeakingUntil))
		speakingChanged := nextSpeaking != entry.session.Speaking
		entry.session.Speaking = nextSpeaking
		entry.mu.Unlock()
		if closed {
			return
		}

		_ = m.audioIO.WritePeerSample(peerID, audio.AudioSample{
			Data:      pkt.Payload,
			Duration:  audio.DefaultFrameDuration,
			Timestamp: now,
			Energy:    energy,
			Sequence:  uint64(pkt.SequenceNumber),
		}, volume, localMuted)

		if speakingChanged {
			m.notifySessionsChanged()
		}
	}
}

func (m *WebRTCMesh) attachDataChannel(entry *peerConnEntry, dc *webrtc.DataChannel) {
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		_ = dc.Close()
		return
	}
	entry.dc = dc
	entry.mu.Unlock()

	dc.OnOpen(func() {
		m.mu.Lock()
		muted := m.localMuted
		speaking := m.speaking
		m.mu.Unlock()

		_ = sendControlJSON(dc, controlMessage{
			Kind:     "state",
			Muted:    muted,
			Speaking: speaking,
		})
		_ = sendControlJSON(dc, controlMessage{
			Kind:     "ping",
			SentAtNs: time.Now().UnixNano(),
		})

		m.wg.Add(1)
		go m.controlPingLoop(entry, dc)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var ctrl controlMessage
		if err := json.Unmarshal(msg.Data, &ctrl); err != nil {
			return
		}
		switch ctrl.Kind {
		case "ping":
			_ = sendControlJSON(dc, controlMessage{
				Kind:     "pong",
				SentAtNs: ctrl.SentAtNs,
			})
		case "pong":
			if ctrl.SentAtNs > 0 {
				elapsed := time.Since(time.Unix(0, ctrl.SentAtNs))
				rttMs := int(elapsed.Milliseconds())
				if rttMs <= 0 {
					rttMs = 1
				}
				entry.mu.Lock()
				entry.session.RTTMs = rttMs
				entry.mu.Unlock()
				m.notifySessionsChanged()
			}
		case "state":
			now := time.Now()
			entry.mu.Lock()
			entry.session.RemoteMuted = ctrl.Muted
			entry.dcSpeaking = ctrl.Speaking && !ctrl.Muted
			entry.session.Speaking = !entry.session.RemoteMuted && (entry.dcSpeaking || now.Before(entry.audioSpeakingUntil))
			entry.mu.Unlock()
			m.notifySessionsChanged()
		}
	})
}

func (m *WebRTCMesh) controlPingLoop(entry *peerConnEntry, dc *webrtc.DataChannel) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-entry.done:
			return
		case now := <-ticker.C:
			entry.mu.Lock()
			nextSpeaking := !entry.session.RemoteMuted && (entry.dcSpeaking || now.Before(entry.audioSpeakingUntil))
			changed := nextSpeaking != entry.session.Speaking
			entry.session.Speaking = nextSpeaking
			entry.mu.Unlock()
			if changed {
				m.notifySessionsChanged()
			}
			if dc.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}
			_ = sendControlJSON(dc, controlMessage{
				Kind:     "ping",
				SentAtNs: time.Now().UnixNano(),
			})
		}
	}
}

func sendControlJSON(dc *webrtc.DataChannel, msg controlMessage) error {
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return nil
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return dc.SendText(string(payload))
}

func (m *WebRTCMesh) handlePCStateChange(entry *peerConnEntry, state webrtc.PeerConnectionState) {
	switch state {
	case webrtc.PeerConnectionStateConnecting:
		entry.mu.Lock()
		if !entry.closed {
			_ = entry.session.TransitionTo(domain.StateConnecting)
		}
		entry.mu.Unlock()
		m.notifySessionsChanged()

	case webrtc.PeerConnectionStateConnected:
		var pair *webrtc.ICECandidatePair
		if iceTransport := entry.pc.SCTP().Transport().ICETransport(); iceTransport != nil {
			selected, err := iceTransport.GetSelectedCandidatePair()
			if err == nil && selected != nil {
				pair = selected
			}
		}
		if pair != nil {
			m.updateSelectedPair(entry, pair)
		} else {
			entry.mu.Lock()
			if !entry.closed && entry.session.State != domain.StateConnectedP2P && entry.session.State != domain.StateConnectedRelay {
				next := domain.StateConnectedP2P
				if m.forceRelay {
					next = domain.StateConnectedRelay
				}
				_ = entry.session.TransitionTo(next)
			}
			entry.mu.Unlock()
			m.notifySessionsChanged()
		}

	case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
		entry.mu.Lock()
		if !entry.closed {
			_ = entry.session.TransitionTo(domain.StateDisconnected)
		}
		entry.mu.Unlock()
		m.notifySessionsChanged()

	case webrtc.PeerConnectionStateClosed:
		entry.mu.Lock()
		_ = entry.session.TransitionTo(domain.StateClosed)
		entry.mu.Unlock()
		m.notifySessionsChanged()
	}
}

func (m *WebRTCMesh) updateSelectedPair(entry *peerConnEntry, pair *webrtc.ICECandidatePair) {
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return
	}
	nextState, err := domain.ClassifyPath(
		domain.CandidateType(pair.Local.Typ.String()),
		domain.CandidateType(pair.Remote.Typ.String()),
	)
	if err != nil {
		return
	}

	entry.mu.Lock()
	if !entry.closed {
		_ = entry.session.TransitionTo(nextState)
	}
	entry.mu.Unlock()
	m.notifySessionsChanged()
}

// SetPeerVolume adjusts the local playback gain (0..200) for a specific remote peer.
func (m *WebRTCMesh) SetPeerVolume(peerID string, volume int) error {
	m.mu.Lock()
	entry, ok := m.peers[peerID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("peer %q not connected", peerID)
	}

	entry.mu.Lock()
	err := entry.session.SetVolume(volume)
	entry.mu.Unlock()
	if err != nil {
		return err
	}
	m.notifySessionsChanged()
	return nil
}

// TogglePeerMute flips the local mute override for a specific remote peer.
func (m *WebRTCMesh) TogglePeerMute(peerID string) (bool, error) {
	m.mu.Lock()
	entry, ok := m.peers[peerID]
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("peer %q not connected", peerID)
	}

	entry.mu.Lock()
	entry.session.LocalMuted = !entry.session.LocalMuted
	muted := entry.session.LocalMuted
	entry.mu.Unlock()
	m.notifySessionsChanged()
	return muted, nil
}

// BroadcastControlState updates local mute/speaking flags and notifies all connected peers over DataChannel.
func (m *WebRTCMesh) BroadcastControlState(muted, speaking bool) {
	m.mu.Lock()
	m.localMuted = muted
	m.speaking = speaking && !muted
	entries := make([]*peerConnEntry, 0, len(m.peers))
	for _, e := range m.peers {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	msg := controlMessage{
		Kind:     "state",
		Muted:    muted,
		Speaking: speaking && !muted,
	}
	for _, entry := range entries {
		entry.mu.Lock()
		dc := entry.dc
		closed := entry.closed
		entry.mu.Unlock()
		if !closed && dc != nil {
			_ = sendControlJSON(dc, msg)
		}
	}
}

// Sessions returns a deterministically sorted snapshot of all active remote peer sessions.
func (m *WebRTCMesh) Sessions() []domain.PeerSession {
	m.mu.Lock()
	entries := make([]*peerConnEntry, 0, len(m.peers))
	for _, e := range m.peers {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	out := make([]domain.PeerSession, 0, len(entries))
	for _, entry := range entries {
		entry.mu.Lock()
		out = append(out, entry.session)
		entry.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b domain.PeerSession) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (m *WebRTCMesh) emitSignal(env domain.SignalEnvelope) error {
	ctx, cancel := context.WithTimeout(m.ctx, signalSendTimeout)
	defer cancel()
	return m.sendSignal(ctx, env)
}

func (m *WebRTCMesh) notifySessionsChanged() {
	if m.onSessionsChange != nil {
		m.onSessionsChange(m.Sessions())
	}
}

// Close closes all PeerConnections, DataChannels, and waits for mesh goroutines to finish.
func (m *WebRTCMesh) Close() error {
	m.cancel()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return nil
	}
	m.closed = true
	entries := make([]*peerConnEntry, 0, len(m.peers))
	for id, entry := range m.peers {
		entries = append(entries, entry)
		delete(m.peers, id)
	}
	m.mu.Unlock()

	for _, entry := range entries {
		entry.closeResources()
	}

	m.wg.Wait()
	m.notifySessionsChanged()
	return nil
}
