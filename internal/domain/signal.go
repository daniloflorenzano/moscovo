package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// SignalType identifies the kind of signaling envelope exchanged over the Hub WebSocket.
type SignalType string

const (
	MsgJoin      SignalType = "join"
	MsgLeave     SignalType = "leave"
	MsgRoster    SignalType = "roster"
	MsgOffer     SignalType = "offer"
	MsgAnswer    SignalType = "answer"
	MsgCandidate SignalType = "candidate"
	MsgError     SignalType = "error"
)

var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// PeerInfo describes a participant registered in a Hub voice room.
type PeerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TURNCredentials holds short-lived TURN relay credentials issued by the Hub.
type TURNCredentials struct {
	URLs      []string `json:"urls"`
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	ExpiresAt int64    `json:"expiresAt"`
}

// Validate checks that TURN credentials contain usable relay endpoints, credentials, and expiry.
func (c TURNCredentials) Validate() error {
	if len(c.URLs) == 0 {
		return errors.New("turn credentials require at least one url")
	}
	for _, u := range c.URLs {
		if strings.TrimSpace(u) == "" {
			return errors.New("turn credentials contain an empty url")
		}
	}
	if strings.TrimSpace(c.Username) == "" {
		return errors.New("turn credentials require a username")
	}
	if strings.TrimSpace(c.Password) == "" {
		return errors.New("turn credentials require a password")
	}
	if c.ExpiresAt <= 0 {
		return errors.New("turn credentials require a positive expiration timestamp")
	}
	return nil
}

// SignalEnvelope is the JSON payload exchanged between clients and the Shared Hub.
type SignalEnvelope struct {
	Type       SignalType       `json:"type"`
	SenderID   string           `json:"senderId,omitempty"`
	SenderName string           `json:"senderName,omitempty"`
	TargetID   string           `json:"targetId,omitempty"`
	SDP        string           `json:"sdp,omitempty"`
	Candidate  string           `json:"candidate,omitempty"`
	Peers      []PeerInfo       `json:"peers,omitempty"`
	TURN       *TURNCredentials `json:"turn,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// Validate enforces protocol invariants on a signaling message before sending or processing.
func (e SignalEnvelope) Validate() error {
	switch e.Type {
	case MsgJoin:
		if strings.TrimSpace(e.SenderID) == "" {
			return errors.New("join requires senderId")
		}
		if strings.TrimSpace(e.SenderName) == "" {
			return errors.New("join requires senderName")
		}
		return nil

	case MsgLeave:
		if strings.TrimSpace(e.SenderID) == "" {
			return errors.New("leave requires senderId")
		}
		return nil

	case MsgRoster:
		seen := make(map[string]struct{}, len(e.Peers))
		for _, p := range e.Peers {
			if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
				return errors.New("roster peer requires non-empty id and name")
			}
			if _, dup := seen[p.ID]; dup {
				return fmt.Errorf("roster contains duplicate peer id %q", p.ID)
			}
			seen[p.ID] = struct{}{}
		}
		if e.TURN != nil {
			if err := e.TURN.Validate(); err != nil {
				return err
			}
		}
		return nil

	case MsgOffer, MsgAnswer:
		if err := validateTargetedPair(e.SenderID, e.TargetID, string(e.Type)); err != nil {
			return err
		}
		if strings.TrimSpace(e.SDP) == "" {
			return fmt.Errorf("%s requires non-empty sdp", e.Type)
		}
		return nil

	case MsgCandidate:
		if err := validateTargetedPair(e.SenderID, e.TargetID, string(e.Type)); err != nil {
			return err
		}
		if strings.TrimSpace(e.Candidate) == "" {
			return errors.New("candidate requires non-empty candidate payload")
		}
		return nil

	case MsgError:
		if strings.TrimSpace(e.Error) == "" {
			return errors.New("error envelope requires non-empty error message")
		}
		return nil

	default:
		return fmt.Errorf("unknown signal type %q", e.Type)
	}
}

func validateTargetedPair(senderID, targetID, msgType string) error {
	if strings.TrimSpace(senderID) == "" {
		return fmt.Errorf("%s requires senderId", msgType)
	}
	if strings.TrimSpace(targetID) == "" {
		return fmt.Errorf("%s requires targetId", msgType)
	}
	if senderID == targetID {
		return fmt.Errorf("%s senderId and targetId cannot match (%q)", msgType, senderID)
	}
	return nil
}

// Encode validates and serializes the envelope to JSON.
func (e SignalEnvelope) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// ParseEnvelope decodes and validates a JSON signaling frame.
func ParseEnvelope(data []byte) (SignalEnvelope, error) {
	var env SignalEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return SignalEnvelope{}, fmt.Errorf("decode signal envelope: %w", err)
	}
	if err := env.Validate(); err != nil {
		return SignalEnvelope{}, err
	}
	return env, nil
}

// ShouldFilterTailscaleHostCandidate reports whether an ICE candidate SDP line
// represents a direct "host" candidate on a Tailscale CGNAT address (100.64.0.0/10).
// Peers on distinct Tailnets cannot reach each other's 100.x.y.z IPs directly,
// whereas "relay", "srflx", and "prflx" candidates remain permitted.
func ShouldFilterTailscaleHostCandidate(candidateSDP string) bool {
	trimmed := strings.TrimSpace(candidateSDP)
	if trimmed == "" {
		return false
	}
	trimmed = strings.TrimPrefix(trimmed, "candidate:")
	fields := strings.Fields(trimmed)
	if len(fields) < 8 {
		return false
	}

	var candType string
	for i := 6; i+1 < len(fields); i++ {
		if fields[i] == "typ" {
			candType = fields[i+1]
			break
		}
	}
	if candType != string(CandidateHost) {
		return false
	}

	addr, err := netip.ParseAddr(fields[4])
	if err != nil {
		return false
	}
	return tailscaleCGNAT.Contains(addr.Unmap())
}
