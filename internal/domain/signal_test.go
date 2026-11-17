package domain

import (
	"testing"
)

func TestTURNCredentialsValidate(t *testing.T) {
	tests := []struct {
		name    string
		creds   TURNCredentials
		wantErr bool
	}{
		{
			name: "valid credentials",
			creds: TURNCredentials{
				URLs:      []string{"turn:100.64.10.1:3478?transport=udp"},
				Username:  "1800000000:alice",
				Password:  "secret-hmac",
				ExpiresAt: 1800000000,
			},
			wantErr: false,
		},
		{
			name: "missing urls",
			creds: TURNCredentials{
				Username:  "1800000000:alice",
				Password:  "secret-hmac",
				ExpiresAt: 1800000000,
			},
			wantErr: true,
		},
		{
			name: "empty url element",
			creds: TURNCredentials{
				URLs:      []string{""},
				Username:  "1800000000:alice",
				Password:  "secret-hmac",
				ExpiresAt: 1800000000,
			},
			wantErr: true,
		},
		{
			name: "missing username",
			creds: TURNCredentials{
				URLs:      []string{"turn:100.64.10.1:3478"},
				Password:  "secret-hmac",
				ExpiresAt: 1800000000,
			},
			wantErr: true,
		},
		{
			name: "missing password",
			creds: TURNCredentials{
				URLs:      []string{"turn:100.64.10.1:3478"},
				Username:  "1800000000:alice",
				ExpiresAt: 1800000000,
			},
			wantErr: true,
		},
		{
			name: "invalid expiry",
			creds: TURNCredentials{
				URLs:      []string{"turn:100.64.10.1:3478"},
				Username:  "1800000000:alice",
				Password:  "secret-hmac",
				ExpiresAt: 0,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.creds.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestSignalEnvelopeValidateAndRoundTrip(t *testing.T) {
	validTURN := &TURNCredentials{
		URLs:      []string{"turn:100.64.10.1:3478?transport=udp"},
		Username:  "1800000000:alice",
		Password:  "hmac-token",
		ExpiresAt: 1800000000,
	}

	tests := []struct {
		name    string
		env     SignalEnvelope
		wantErr bool
	}{
		{
			name: "valid join",
			env: SignalEnvelope{
				Type:       MsgJoin,
				SenderID:   "peer-a",
				SenderName: "alice",
			},
			wantErr: false,
		},
		{
			name: "join missing sender id",
			env: SignalEnvelope{
				Type:       MsgJoin,
				SenderName: "alice",
			},
			wantErr: true,
		},
		{
			name: "join missing sender name",
			env: SignalEnvelope{
				Type:     MsgJoin,
				SenderID: "peer-a",
			},
			wantErr: true,
		},
		{
			name: "valid leave",
			env: SignalEnvelope{
				Type:     MsgLeave,
				SenderID: "peer-a",
			},
			wantErr: false,
		},
		{
			name: "leave missing sender id",
			env: SignalEnvelope{
				Type: MsgLeave,
			},
			wantErr: true,
		},
		{
			name: "valid roster with turn credentials",
			env: SignalEnvelope{
				Type: MsgRoster,
				Peers: []PeerInfo{
					{ID: "peer-a", Name: "alice"},
					{ID: "peer-b", Name: "bob"},
				},
				TURN: validTURN,
			},
			wantErr: false,
		},
		{
			name: "roster with duplicate peers",
			env: SignalEnvelope{
				Type: MsgRoster,
				Peers: []PeerInfo{
					{ID: "peer-a", Name: "alice"},
					{ID: "peer-a", Name: "alice2"},
				},
			},
			wantErr: true,
		},
		{
			name: "roster with invalid peer entry",
			env: SignalEnvelope{
				Type: MsgRoster,
				Peers: []PeerInfo{
					{ID: "", Name: "alice"},
				},
			},
			wantErr: true,
		},
		{
			name: "roster with invalid turn credentials",
			env: SignalEnvelope{
				Type: MsgRoster,
				Peers: []PeerInfo{
					{ID: "peer-a", Name: "alice"},
				},
				TURN: &TURNCredentials{Username: "bad"},
			},
			wantErr: true,
		},
		{
			name: "valid offer",
			env: SignalEnvelope{
				Type:     MsgOffer,
				SenderID: "peer-a",
				TargetID: "peer-b",
				SDP:      "v=0\r\no=- 123 2 IN IP4 127.0.0.1\r\n",
			},
			wantErr: false,
		},
		{
			name: "offer missing target",
			env: SignalEnvelope{
				Type:     MsgOffer,
				SenderID: "peer-a",
				SDP:      "v=0\r\n",
			},
			wantErr: true,
		},
		{
			name: "offer self targeted",
			env: SignalEnvelope{
				Type:     MsgOffer,
				SenderID: "peer-a",
				TargetID: "peer-a",
				SDP:      "v=0\r\n",
			},
			wantErr: true,
		},
		{
			name: "answer empty sdp",
			env: SignalEnvelope{
				Type:     MsgAnswer,
				SenderID: "peer-b",
				TargetID: "peer-a",
				SDP:      "   ",
			},
			wantErr: true,
		},
		{
			name: "valid answer",
			env: SignalEnvelope{
				Type:     MsgAnswer,
				SenderID: "peer-b",
				TargetID: "peer-a",
				SDP:      "v=0\r\n",
			},
			wantErr: false,
		},
		{
			name: "valid candidate",
			env: SignalEnvelope{
				Type:      MsgCandidate,
				SenderID:  "peer-a",
				TargetID:  "peer-b",
				Candidate: `{"candidate":"candidate:1 1 udp 2122260223 192.168.1.10 50000 typ host"}`,
			},
			wantErr: false,
		},
		{
			name: "candidate missing payload",
			env: SignalEnvelope{
				Type:     MsgCandidate,
				SenderID: "peer-a",
				TargetID: "peer-b",
			},
			wantErr: true,
		},
		{
			name: "valid error envelope",
			env: SignalEnvelope{
				Type:  MsgError,
				Error: "duplicate peer id",
			},
			wantErr: false,
		},
		{
			name: "error envelope empty message",
			env: SignalEnvelope{
				Type: MsgError,
			},
			wantErr: true,
		},
		{
			name: "unknown type",
			env: SignalEnvelope{
				Type: "ping",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.env.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}

			encoded, encErr := tc.env.Encode()
			if tc.wantErr {
				if encErr == nil {
					t.Fatalf("Encode() succeeded for invalid envelope, expected error")
				}
				return
			}
			if encErr != nil {
				t.Fatalf("Encode() unexpected error: %v", encErr)
			}

			decoded, decErr := ParseEnvelope(encoded)
			if decErr != nil {
				t.Fatalf("ParseEnvelope() unexpected error: %v", decErr)
			}
			if decoded.Type != tc.env.Type || decoded.SenderID != tc.env.SenderID || decoded.TargetID != tc.env.TargetID {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", decoded, tc.env)
			}
		})
	}
}

func TestParseEnvelopeInvalidJSON(t *testing.T) {
	if _, err := ParseEnvelope([]byte("{not-json")); err == nil {
		t.Fatalf("ParseEnvelope() expected error for malformed JSON")
	}
}

func TestShouldFilterTailscaleHostCandidate(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{
			name:      "tailscale cgnat host candidate filtered",
			candidate: "candidate:1 1 udp 2122260223 100.64.10.5 54321 typ host generation 0",
			want:      true,
		},
		{
			name:      "tailscale cgnat upper range host candidate filtered",
			candidate: "1 1 udp 2122260223 100.100.1.2 40000 typ host",
			want:      true,
		},
		{
			name:      "tailscale cgnat boundary start",
			candidate: "candidate:1 1 udp 2122260223 100.64.0.1 12345 typ host",
			want:      true,
		},
		{
			name:      "tailscale cgnat boundary end",
			candidate: "candidate:1 1 udp 2122260223 100.127.255.254 12345 typ host",
			want:      true,
		},
		{
			name:      "non-tailscale 100.x outside cgnat kept",
			candidate: "candidate:1 1 udp 2122260223 100.128.0.1 12345 typ host",
			want:      false,
		},
		{
			name:      "hub relay on tailscale cgnat kept",
			candidate: "candidate:2 1 udp 16777215 100.64.10.1 49152 typ relay raddr 0.0.0.0 rport 0",
			want:      false,
		},
		{
			name:      "srflx candidate kept",
			candidate: "candidate:3 1 udp 1686052607 203.0.113.45 54321 typ srflx raddr 192.168.1.10 rport 54321",
			want:      false,
		},
		{
			name:      "prflx candidate kept",
			candidate: "candidate:4 1 udp 1853817087 203.0.113.45 54321 typ prflx raddr 192.168.1.10 rport 54321",
			want:      false,
		},
		{
			name:      "lan host candidate kept",
			candidate: "candidate:5 1 udp 2122260223 192.168.1.10 50000 typ host",
			want:      false,
		},
		{
			name:      "loopback host candidate kept",
			candidate: "candidate:6 1 udp 2122260223 127.0.0.1 50000 typ host",
			want:      false,
		},
		{
			name:      "empty candidate string ignored safely",
			candidate: "",
			want:      false,
		},
		{
			name:      "malformed candidate string ignored safely",
			candidate: "candidate:bad",
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldFilterTailscaleHostCandidate(tc.candidate); got != tc.want {
				t.Fatalf("ShouldFilterTailscaleHostCandidate(%q) = %v, want %v", tc.candidate, got, tc.want)
			}
		})
	}
}
