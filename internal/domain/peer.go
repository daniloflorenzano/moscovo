package domain

import (
	"errors"
	"fmt"
	"strings"
)

// CandidateType classifies an ICE candidate transport path.
type CandidateType string

const (
	CandidateHost  CandidateType = "host"
	CandidateSrflx CandidateType = "srflx"
	CandidatePrflx CandidateType = "prflx"
	CandidateRelay CandidateType = "relay"
)

// PeerState represents the lifecycle and transport path of a remote peer session.
type PeerState string

const (
	StateIdle           PeerState = "IDLE"
	StateSignaling      PeerState = "SIGNALING"
	StateConnecting     PeerState = "CONNECTING"
	StateConnectedP2P   PeerState = "P2P"
	StateConnectedRelay PeerState = "TURN"
	StateDisconnected   PeerState = "DISCONNECTED"
	StateClosed         PeerState = "CLOSED"
)

func isValidCandidateType(c CandidateType) bool {
	switch c {
	case CandidateHost, CandidateSrflx, CandidatePrflx, CandidateRelay:
		return true
	default:
		return false
	}
}

// ClassifyPath determines whether an active ICE candidate pair is direct P2P or Hub-relayed TURN.
func ClassifyPath(localCand, remoteCand CandidateType) (PeerState, error) {
	if !isValidCandidateType(localCand) {
		return "", fmt.Errorf("unknown local candidate type %q", localCand)
	}
	if !isValidCandidateType(remoteCand) {
		return "", fmt.Errorf("unknown remote candidate type %q", remoteCand)
	}
	if localCand == CandidateRelay || remoteCand == CandidateRelay {
		return StateConnectedRelay, nil
	}
	return StateConnectedP2P, nil
}

var allowedTransitions = map[PeerState][]PeerState{
	StateIdle:           {StateSignaling, StateConnecting, StateDisconnected, StateClosed},
	StateSignaling:      {StateConnecting, StateConnectedP2P, StateConnectedRelay, StateDisconnected, StateClosed},
	StateConnecting:     {StateSignaling, StateConnectedP2P, StateConnectedRelay, StateDisconnected, StateClosed},
	StateConnectedP2P:   {StateConnectedP2P, StateConnectedRelay, StateDisconnected, StateClosed},
	StateConnectedRelay: {StateConnectedP2P, StateConnectedRelay, StateDisconnected, StateClosed},
	StateDisconnected:   {StateSignaling, StateConnecting, StateConnectedP2P, StateConnectedRelay, StateClosed},
	StateClosed:         nil,
}

// PeerSession captures the state and audio/screen-share status of a remote peer.
type PeerSession struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	State         PeerState `json:"state"`
	Volume        int       `json:"volume"`
	LocalMuted    bool      `json:"localMuted"`
	RemoteMuted   bool      `json:"remoteMuted"`
	Speaking      bool      `json:"speaking"`
	RTTMs         int       `json:"rttMs"`
	ScreenSharing bool      `json:"screenSharing"`
	ScreenAudio   bool      `json:"screenAudio,omitempty"`
}

// NewPeerSession constructs a peer session starting in StateIdle at 100% local volume.
func NewPeerSession(id, name string) (PeerSession, error) {
	if strings.TrimSpace(id) == "" {
		return PeerSession{}, errors.New("peer id cannot be empty")
	}
	if strings.TrimSpace(name) == "" {
		return PeerSession{}, errors.New("peer name cannot be empty")
	}
	return PeerSession{
		ID:     id,
		Name:   name,
		State:  StateIdle,
		Volume: 100,
	}, nil
}

// TransitionTo advances the session to next if the state machine permits the transition.
func (p *PeerSession) TransitionTo(next PeerState) error {
	allowed := allowedTransitions[p.State]
	for _, candidate := range allowed {
		if candidate == next {
			p.State = next
			return nil
		}
	}
	return fmt.Errorf("invalid peer state transition %q -> %q", p.State, next)
}

// SetVolume updates the local playback gain percentage (0..200).
func (p *PeerSession) SetVolume(volume int) error {
	if volume < 0 || volume > 200 {
		return fmt.Errorf("volume %d out of range [0, 200]", volume)
	}
	p.Volume = volume
	return nil
}

// NoiseSuppressionMode selects the client-side microphone noise suppressor algorithm.
type NoiseSuppressionMode string

const (
	NoiseSuppressionOff     NoiseSuppressionMode = "off"
	NoiseSuppressionRNNoise NoiseSuppressionMode = "rnnoise"
	NoiseSuppressionSpeex   NoiseSuppressionMode = "speex"
	NoiseSuppressionGate    NoiseSuppressionMode = "gate"

	DefaultNoiseSuppressionMode = NoiseSuppressionRNNoise
)

// IsValidNoiseSuppressionMode reports whether m is a supported noise suppression mode.
func IsValidNoiseSuppressionMode(m NoiseSuppressionMode) bool {
	switch m {
	case NoiseSuppressionOff, NoiseSuppressionRNNoise, NoiseSuppressionSpeex, NoiseSuppressionGate:
		return true
	default:
		return false
	}
}

// NextNoiseSuppressionMode returns the next mode in the cycle: off -> rnnoise -> speex -> gate -> off.
func NextNoiseSuppressionMode(current NoiseSuppressionMode) NoiseSuppressionMode {
	switch current {
	case NoiseSuppressionOff:
		return NoiseSuppressionRNNoise
	case NoiseSuppressionRNNoise:
		return NoiseSuppressionSpeex
	case NoiseSuppressionSpeex:
		return NoiseSuppressionGate
	case NoiseSuppressionGate:
		return NoiseSuppressionOff
	default:
		return DefaultNoiseSuppressionMode
	}
}
