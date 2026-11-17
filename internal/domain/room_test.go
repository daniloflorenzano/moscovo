package domain

import (
	"reflect"
	"testing"
)

func TestRoomMembershipInvariants(t *testing.T) {
	room := NewRoom()

	if err := room.Join(PeerInfo{ID: "", Name: "alice"}); err == nil {
		t.Fatalf("expected error when joining with empty ID")
	}
	if err := room.Join(PeerInfo{ID: "peer-a", Name: ""}); err == nil {
		t.Fatalf("expected error when joining with empty Name")
	}

	if err := room.Join(PeerInfo{ID: "peer-c", Name: "charlie"}); err != nil {
		t.Fatalf("unexpected error joining peer-c: %v", err)
	}
	if err := room.Join(PeerInfo{ID: "peer-a", Name: "alice"}); err != nil {
		t.Fatalf("unexpected error joining peer-a: %v", err)
	}
	if err := room.Join(PeerInfo{ID: "peer-b", Name: "bob"}); err != nil {
		t.Fatalf("unexpected error joining peer-b: %v", err)
	}

	if err := room.Join(PeerInfo{ID: "peer-a", Name: "alice-dup"}); err == nil {
		t.Fatalf("expected error on duplicate peer ID")
	}

	if !room.HasPeer("peer-b") {
		t.Fatalf("HasPeer(peer-b) = false, want true")
	}
	if room.HasPeer("peer-unknown") {
		t.Fatalf("HasPeer(peer-unknown) = true, want false")
	}

	wantOrder := []PeerInfo{
		{ID: "peer-a", Name: "alice"},
		{ID: "peer-b", Name: "bob"},
		{ID: "peer-c", Name: "charlie"},
	}
	if got := room.Peers(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("Peers() = %+v, want %+v", got, wantOrder)
	}

	if err := room.Leave(""); err == nil {
		t.Fatalf("expected error leaving with empty peer ID")
	}
	if err := room.Leave("peer-unknown"); err == nil {
		t.Fatalf("expected error leaving unknown peer")
	}
	if err := room.Leave("peer-b"); err != nil {
		t.Fatalf("unexpected error leaving peer-b: %v", err)
	}
	if room.HasPeer("peer-b") {
		t.Fatalf("peer-b still in room after Leave")
	}

	wantAfterLeave := []PeerInfo{
		{ID: "peer-a", Name: "alice"},
		{ID: "peer-c", Name: "charlie"},
	}
	if got := room.Peers(); !reflect.DeepEqual(got, wantAfterLeave) {
		t.Fatalf("Peers() after Leave = %+v, want %+v", got, wantAfterLeave)
	}
}

func TestShouldInitiateOffer(t *testing.T) {
	tests := []struct {
		name     string
		localID  string
		remoteID string
		want     bool
		wantErr  bool
	}{
		{
			name:     "lower local id initiates offer",
			localID:  "peer-a",
			remoteID: "peer-b",
			want:     true,
		},
		{
			name:     "higher local id waits for offer",
			localID:  "peer-b",
			remoteID: "peer-a",
			want:     false,
		},
		{
			name:     "identical ids rejected",
			localID:  "peer-a",
			remoteID: "peer-a",
			wantErr:  true,
		},
		{
			name:     "empty local id rejected",
			localID:  "",
			remoteID: "peer-b",
			wantErr:  true,
		},
		{
			name:     "empty remote id rejected",
			localID:  "peer-a",
			remoteID: "",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ShouldInitiateOffer(tc.localID, tc.remoteID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ShouldInitiateOffer() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("ShouldInitiateOffer(%q, %q) = %v, want %v", tc.localID, tc.remoteID, got, tc.want)
			}
		})
	}
}

func TestComputeMeshDiff(t *testing.T) {
	tests := []struct {
		name      string
		localID   string
		current   []string
		roster    []PeerInfo
		want      MeshDiff
		wantError bool
	}{
		{
			name:    "2-peer initial join from lower id",
			localID: "peer-a",
			current: nil,
			roster: []PeerInfo{
				{ID: "peer-a", Name: "alice"},
				{ID: "peer-b", Name: "bob"},
			},
			want: MeshDiff{
				ToConnect: []ConnectTarget{
					{Peer: PeerInfo{ID: "peer-b", Name: "bob"}, InitiateOffer: true},
				},
			},
		},
		{
			name:    "3rd peer joins room viewed by middle peer",
			localID: "peer-b",
			current: []string{"peer-a"},
			roster: []PeerInfo{
				{ID: "peer-a", Name: "alice"},
				{ID: "peer-b", Name: "bob"},
				{ID: "peer-c", Name: "charlie"},
			},
			want: MeshDiff{
				ToConnect: []ConnectTarget{
					{Peer: PeerInfo{ID: "peer-c", Name: "charlie"}, InitiateOffer: true},
				},
			},
		},
		{
			name:    "3rd peer joins room viewed by new peer-c (polite to a and b)",
			localID: "peer-c",
			current: nil,
			roster: []PeerInfo{
				{ID: "peer-c", Name: "charlie"},
				{ID: "peer-b", Name: "bob"},
				{ID: "peer-a", Name: "alice"},
			},
			want: MeshDiff{
				ToConnect: []ConnectTarget{
					{Peer: PeerInfo{ID: "peer-a", Name: "alice"}, InitiateOffer: false},
					{Peer: PeerInfo{ID: "peer-b", Name: "bob"}, InitiateOffer: false},
				},
			},
		},
		{
			name:    "peer leaves while another peer joins",
			localID: "peer-b",
			current: []string{"peer-a", "peer-d"},
			roster: []PeerInfo{
				{ID: "peer-b", Name: "bob"},
				{ID: "peer-c", Name: "charlie"},
				{ID: "peer-d", Name: "dave"},
			},
			want: MeshDiff{
				ToConnect: []ConnectTarget{
					{Peer: PeerInfo{ID: "peer-c", Name: "charlie"}, InitiateOffer: true},
				},
				ToDisconnect: []string{"peer-a"},
			},
		},
		{
			name:    "empty roster disconnects all peers",
			localID: "peer-a",
			current: []string{"peer-c", "peer-b"},
			roster:  nil,
			want: MeshDiff{
				ToDisconnect: []string{"peer-b", "peer-c"},
			},
		},
		{
			name:      "empty local id rejected",
			localID:   "",
			wantError: true,
		},
		{
			name:    "duplicate peer in roster rejected",
			localID: "peer-a",
			roster: []PeerInfo{
				{ID: "peer-b", Name: "bob"},
				{ID: "peer-b", Name: "bob2"},
			},
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComputeMeshDiff(tc.localID, tc.current, tc.roster)
			if (err != nil) != tc.wantError {
				t.Fatalf("ComputeMeshDiff() error = %v, wantError = %v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			if !reflect.DeepEqual(got.ToConnect, tc.want.ToConnect) {
				t.Fatalf("ToConnect = %+v, want %+v", got.ToConnect, tc.want.ToConnect)
			}
			if !reflect.DeepEqual(got.ToDisconnect, tc.want.ToDisconnect) {
				t.Fatalf("ToDisconnect = %+v, want %+v", got.ToDisconnect, tc.want.ToDisconnect)
			}
		})
	}
}
