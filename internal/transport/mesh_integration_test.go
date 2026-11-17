package transport

import (
	"context"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"moscovo/internal/audio"
	"moscovo/internal/domain"
	"moscovo/internal/hub"
)

type testPeerNode struct {
	id     string
	audio  *audio.Engine
	client *HubClient
	mesh   *WebRTCMesh
}

func startTestHub(t *testing.T) (*hub.TURNServer, *hub.Server, string) {
	t.Helper()

	turnSrv, err := hub.NewTURNServer(hub.TURNConfig{
		ListenIP: "127.0.0.1",
		RelayIP:  "127.0.0.1",
		Port:     0,
		Realm:    "moscovo.test",
	})
	if err != nil {
		t.Fatalf("NewTURNServer() failed: %v", err)
	}

	hubSrv := hub.NewServer(hub.ServerConfig{
		TURN:         turnSrv,
		PingInterval: 2 * time.Second,
		PongWait:     6 * time.Second,
		WriteWait:    2 * time.Second,
	})
	httpSrv := httptest.NewServer(hubSrv.Handler())

	t.Cleanup(func() {
		httpSrv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hubSrv.Shutdown(ctx)
		_ = turnSrv.Close()
	})

	return turnSrv, hubSrv, httpSrv.URL
}

func startTestPeer(t *testing.T, hubURL, id, name string, forceRelay bool) *testPeerNode {
	t.Helper()

	eng := audio.NewEngine(audio.EngineConfig{
		FrameDuration: 20 * time.Millisecond,
		MicSource: func() ([]byte, float64) {
			return []byte{0xF8, 0xFF, 0xFE, 0x01, 0x02}, 0.25
		},
	})

	var meshMu sync.Mutex
	var mesh *WebRTCMesh

	client, err := NewHubClient(HubClientConfig{
		HubURL: hubURL,
		Peer:   domain.PeerInfo{ID: id, Name: name},
		OnEnvelope: func(env domain.SignalEnvelope) {
			meshMu.Lock()
			m := mesh
			meshMu.Unlock()
			if m == nil {
				return
			}
			switch env.Type {
			case domain.MsgRoster:
				_ = m.ApplyRoster(env.Peers, env.TURN)
			case domain.MsgOffer, domain.MsgAnswer, domain.MsgCandidate:
				_ = m.HandleSignal(env)
			}
		},
	})
	if err != nil {
		t.Fatalf("NewHubClient(%s) failed: %v", id, err)
	}

	m, err := NewWebRTCMesh(MeshConfig{
		LocalPeer:       domain.PeerInfo{ID: id, Name: name},
		STUNServers:     []string{},
		ForceRelay:      forceRelay,
		IncludeLoopback: true,
		PingInterval:    100 * time.Millisecond,
		Audio:           eng,
		SendSignal: func(ctx context.Context, env domain.SignalEnvelope) error {
			return client.Send(ctx, env)
		},
	})
	if err != nil {
		_ = eng.Close()
		t.Fatalf("NewWebRTCMesh(%s) failed: %v", id, err)
	}

	meshMu.Lock()
	mesh = m
	meshMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		_ = m.Close()
		_ = eng.Close()
		t.Fatalf("client.Connect(%s) failed: %v", id, err)
	}

	node := &testPeerNode{
		id:     id,
		audio:  eng,
		client: client,
		mesh:   m,
	}
	t.Cleanup(func() {
		node.close()
	})
	return node
}

func (n *testPeerNode) close() {
	_ = n.client.LeaveAndClose()
	_ = n.mesh.Close()
	_ = n.audio.Close()
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for: %s", timeout, desc)
}

func TestThreePeerFullMeshP2P(t *testing.T) {
	_, _, hubURL := startTestHub(t)

	alice := startTestPeer(t, hubURL, "peer-a", "alice", false)
	bob := startTestPeer(t, hubURL, "peer-b", "bob", false)
	charlie := startTestPeer(t, hubURL, "peer-c", "charlie", false)

	nodes := []*testPeerNode{alice, bob, charlie}
	waitUntil(t, 12*time.Second, "all 3 peers connected P2P with RTT and audio", func() bool {
		for _, n := range nodes {
			sessions := n.mesh.Sessions()
			if len(sessions) != 2 {
				return false
			}
			for _, s := range sessions {
				if s.State != domain.StateConnectedP2P || s.RTTMs <= 0 {
					return false
				}
				stats, ok := n.audio.PeerStats(s.ID)
				if !ok || stats.PacketsReceived == 0 {
					return false
				}
			}
		}
		return true
	})

	alice.mesh.BroadcastControlState(true, false)
	waitUntil(t, 3*time.Second, "bob observes alice muted over DataChannel", func() bool {
		for _, s := range bob.mesh.Sessions() {
			if s.ID == "peer-a" {
				return s.RemoteMuted
			}
		}
		return false
	})
}

func TestForcedTURNRelayFallback(t *testing.T) {
	_, _, hubURL := startTestHub(t)

	alice := startTestPeer(t, hubURL, "peer-a", "alice", true)
	bob := startTestPeer(t, hubURL, "peer-b", "bob", true)

	nodes := []*testPeerNode{alice, bob}
	waitUntil(t, 15*time.Second, "both peers connected via TURN relay with RTT and audio", func() bool {
		for _, n := range nodes {
			sessions := n.mesh.Sessions()
			if len(sessions) != 1 {
				return false
			}
			s := sessions[0]
			if s.State != domain.StateConnectedRelay || s.RTTMs <= 0 {
				return false
			}
			stats, ok := n.audio.PeerStats(s.ID)
			if !ok || stats.PacketsReceived == 0 {
				return false
			}
		}
		return true
	})
}

func TestPeerLeaveRecoveryAndCleanShutdown(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()
	turnSrv, hubSrv, hubURL := startTestHub(t)

	httpSrv := httptest.NewServer(hubSrv.Handler())
	defer httpSrv.Close()
	_ = turnSrv
	_ = hubURL

	alice := startTestPeer(t, httpSrv.URL, "peer-a", "alice", false)
	bob := startTestPeer(t, httpSrv.URL, "peer-b", "bob", false)
	charlie := startTestPeer(t, httpSrv.URL, "peer-c", "charlie", false)

	waitUntil(t, 10*time.Second, "3-peer mesh established", func() bool {
		return len(alice.mesh.Sessions()) == 2 && len(bob.mesh.Sessions()) == 2
	})

	// Charlie leaves abruptly/cleanly; Alice and Bob must prune Charlie from their meshes.
	charlie.close()

	waitUntil(t, 5*time.Second, "alice and bob prune departed peer-c", func() bool {
		as := alice.mesh.Sessions()
		bs := bob.mesh.Sessions()
		return len(as) == 1 && as[0].ID == "peer-b" && len(bs) == 1 && bs[0].ID == "peer-a"
	})

	alice.close()
	bob.close()
	httpSrv.Close()

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := hubSrv.Shutdown(shutCtx); err != nil {
		t.Fatalf("hubSrv.Shutdown() failed: %v", err)
	}
	if err := turnSrv.Close(); err != nil {
		t.Fatalf("turnSrv.Close() failed: %v", err)
	}

	// Allow runtime finalizers / UDP socket close loops brief settle time.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseGoroutines+6 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("goroutines before=%d after=%d", baseGoroutines, runtime.NumGoroutine())
}
