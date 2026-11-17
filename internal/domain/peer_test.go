package domain

import "testing"

func TestClassifyPath(t *testing.T) {
	tests := []struct {
		name      string
		local     CandidateType
		remote    CandidateType
		wantState PeerState
		wantErr   bool
	}{
		{
			name:      "srflx to srflx is direct p2p",
			local:     CandidateSrflx,
			remote:    CandidateSrflx,
			wantState: StateConnectedP2P,
		},
		{
			name:      "host to srflx is direct p2p",
			local:     CandidateHost,
			remote:    CandidateSrflx,
			wantState: StateConnectedP2P,
		},
		{
			name:      "prflx to host is direct p2p",
			local:     CandidatePrflx,
			remote:    CandidateHost,
			wantState: StateConnectedP2P,
		},
		{
			name:      "local relay is turn relay",
			local:     CandidateRelay,
			remote:    CandidateSrflx,
			wantState: StateConnectedRelay,
		},
		{
			name:      "remote relay is turn relay",
			local:     CandidateHost,
			remote:    CandidateRelay,
			wantState: StateConnectedRelay,
		},
		{
			name:      "both relay is turn relay",
			local:     CandidateRelay,
			remote:    CandidateRelay,
			wantState: StateConnectedRelay,
		},
		{
			name:    "unknown local candidate rejected",
			local:   CandidateType("unknown"),
			remote:  CandidateHost,
			wantErr: true,
		},
		{
			name:    "empty remote candidate rejected",
			local:   CandidateHost,
			remote:  CandidateType(""),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyPath(tc.local, tc.remote)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ClassifyPath() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.wantState {
				t.Fatalf("ClassifyPath(%q, %q) = %q, want %q", tc.local, tc.remote, got, tc.wantState)
			}
		})
	}
}

func TestPeerSessionStateMachineAndVolume(t *testing.T) {
	if _, err := NewPeerSession("", "alice"); err == nil {
		t.Fatalf("expected error for empty peer ID")
	}
	if _, err := NewPeerSession("peer-a", ""); err == nil {
		t.Fatalf("expected error for empty peer Name")
	}

	session, err := NewPeerSession("peer-a", "alice")
	if err != nil {
		t.Fatalf("NewPeerSession() unexpected error: %v", err)
	}
	if session.State != StateIdle {
		t.Fatalf("initial State = %q, want %q", session.State, StateIdle)
	}
	if session.Volume != 100 {
		t.Fatalf("initial Volume = %d, want 100", session.Volume)
	}

	validLifecycle := []PeerState{
		StateSignaling,
		StateConnecting,
		StateConnectedP2P,
		StateConnectedRelay,
		StateDisconnected,
		StateClosed,
	}
	for _, next := range validLifecycle {
		if err := session.TransitionTo(next); err != nil {
			t.Fatalf("TransitionTo(%q) failed from %q: %v", next, session.State, err)
		}
		if session.State != next {
			t.Fatalf("State = %q, want %q", session.State, next)
		}
	}

	// CLOSED is terminal: any transition out must fail.
	for _, target := range []PeerState{StateIdle, StateSignaling, StateConnecting, StateConnectedP2P, StateDisconnected, StateClosed} {
		if err := session.TransitionTo(target); err == nil {
			t.Fatalf("TransitionTo(%q) from CLOSED succeeded, expected error", target)
		}
	}

	// Cannot transition back to IDLE from Signaling or Connecting.
	s2, err := NewPeerSession("peer-b", "bob")
	if err != nil {
		t.Fatalf("NewPeerSession() unexpected error: %v", err)
	}
	if err := s2.TransitionTo(StateSignaling); err != nil {
		t.Fatalf("TransitionTo(SIGNALING) failed: %v", err)
	}
	if err := s2.TransitionTo(StateIdle); err == nil {
		t.Fatalf("TransitionTo(IDLE) from SIGNALING succeeded, expected error")
	}

	// Volume invariants.
	if err := s2.SetVolume(0); err != nil {
		t.Fatalf("SetVolume(0) failed: %v", err)
	}
	if err := s2.SetVolume(200); err != nil {
		t.Fatalf("SetVolume(200) failed: %v", err)
	}
	if err := s2.SetVolume(-1); err == nil {
		t.Fatalf("SetVolume(-1) succeeded, expected error")
	}
	if err := s2.SetVolume(201); err == nil {
		t.Fatalf("SetVolume(201) succeeded, expected error")
	}
}

func TestNoiseSuppressionModeValidationAndCycle(t *testing.T) {
	validTests := []struct {
		mode NoiseSuppressionMode
		want bool
	}{
		{NoiseSuppressionOff, true},
		{NoiseSuppressionRNNoise, true},
		{NoiseSuppressionSpeex, true},
		{NoiseSuppressionGate, true},
		{NoiseSuppressionMode(""), false},
		{NoiseSuppressionMode("gtcrn"), false},
		{NoiseSuppressionMode("RNNOISE"), false},
	}

	for _, tc := range validTests {
		if got := IsValidNoiseSuppressionMode(tc.mode); got != tc.want {
			t.Fatalf("IsValidNoiseSuppressionMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}

	cycleTests := []struct {
		current NoiseSuppressionMode
		want    NoiseSuppressionMode
	}{
		{NoiseSuppressionOff, NoiseSuppressionRNNoise},
		{NoiseSuppressionRNNoise, NoiseSuppressionSpeex},
		{NoiseSuppressionSpeex, NoiseSuppressionGate},
		{NoiseSuppressionGate, NoiseSuppressionOff},
		{NoiseSuppressionMode(""), NoiseSuppressionRNNoise},
		{NoiseSuppressionMode("unknown"), NoiseSuppressionRNNoise},
	}

	for _, tc := range cycleTests {
		if got := NextNoiseSuppressionMode(tc.current); got != tc.want {
			t.Fatalf("NextNoiseSuppressionMode(%q) = %q, want %q", tc.current, got, tc.want)
		}
	}
}
