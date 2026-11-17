package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Room tracks active participants in a voice room with deterministic ordering.
type Room struct {
	peers map[string]PeerInfo
}

// NewRoom creates an empty voice room roster.
func NewRoom() *Room {
	return &Room{
		peers: make(map[string]PeerInfo),
	}
}

// Join adds a peer to the room, rejecting empty identifiers or duplicate IDs.
func (r *Room) Join(peer PeerInfo) error {
	if strings.TrimSpace(peer.ID) == "" {
		return errors.New("peer id cannot be empty")
	}
	if strings.TrimSpace(peer.Name) == "" {
		return errors.New("peer name cannot be empty")
	}
	if _, exists := r.peers[peer.ID]; exists {
		return fmt.Errorf("duplicate peer id %q", peer.ID)
	}
	r.peers[peer.ID] = peer
	return nil
}

// Leave removes a peer from the room by ID.
func (r *Room) Leave(peerID string) error {
	if strings.TrimSpace(peerID) == "" {
		return errors.New("peer id cannot be empty")
	}
	if _, exists := r.peers[peerID]; !exists {
		return fmt.Errorf("peer %q not in room", peerID)
	}
	delete(r.peers, peerID)
	return nil
}

// HasPeer reports whether peerID is currently in the room.
func (r *Room) HasPeer(peerID string) bool {
	_, exists := r.peers[peerID]
	return exists
}

// Peers returns a snapshot of room participants sorted lexicographically by ID.
func (r *Room) Peers() []PeerInfo {
	out := make([]PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b PeerInfo) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// ShouldInitiateOffer enforces the deterministic glare-free initiator rule:
// the peer with the lexicographically smaller ID creates the SDP Offer.
func ShouldInitiateOffer(localID, remoteID string) (bool, error) {
	if strings.TrimSpace(localID) == "" {
		return false, errors.New("local id cannot be empty")
	}
	if strings.TrimSpace(remoteID) == "" {
		return false, errors.New("remote id cannot be empty")
	}
	if localID == remoteID {
		return false, fmt.Errorf("local and remote ids cannot be equal (%q)", localID)
	}
	return localID < remoteID, nil
}

// ConnectTarget describes a remote peer that requires a new WebRTC PeerConnection.
type ConnectTarget struct {
	Peer          PeerInfo
	InitiateOffer bool
}

// MeshDiff describes which PeerConnections to add or close after a roster update.
type MeshDiff struct {
	ToConnect    []ConnectTarget
	ToDisconnect []string
}

// ComputeMeshDiff calculates the deterministic diff between currently connected
// remote peer IDs and the latest Hub roster snapshot.
func ComputeMeshDiff(localID string, currentRemoteIDs []string, roster []PeerInfo) (MeshDiff, error) {
	if strings.TrimSpace(localID) == "" {
		return MeshDiff{}, errors.New("local id cannot be empty")
	}

	desired := make(map[string]PeerInfo, len(roster))
	for _, p := range roster {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
			return MeshDiff{}, errors.New("roster entry requires non-empty id and name")
		}
		if _, dup := desired[p.ID]; dup {
			return MeshDiff{}, fmt.Errorf("duplicate peer id %q in roster", p.ID)
		}
		if p.ID == localID {
			continue
		}
		desired[p.ID] = p
	}

	current := make(map[string]struct{}, len(currentRemoteIDs))
	var diff MeshDiff
	for _, id := range currentRemoteIDs {
		current[id] = struct{}{}
		if _, keep := desired[id]; !keep {
			diff.ToDisconnect = append(diff.ToDisconnect, id)
		}
	}
	slices.Sort(diff.ToDisconnect)

	for id, peer := range desired {
		if _, exists := current[id]; exists {
			continue
		}
		initiate, err := ShouldInitiateOffer(localID, id)
		if err != nil {
			return MeshDiff{}, err
		}
		diff.ToConnect = append(diff.ToConnect, ConnectTarget{
			Peer:          peer,
			InitiateOffer: initiate,
		})
	}
	slices.SortFunc(diff.ToConnect, func(a, b ConnectTarget) int {
		return strings.Compare(a.Peer.ID, b.Peer.ID)
	})

	return diff, nil
}
