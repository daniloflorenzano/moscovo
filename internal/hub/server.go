package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"moscovo/internal/domain"
)

const (
	defaultPingInterval = 10 * time.Second
	defaultPongWait     = 25 * time.Second
	defaultWriteWait    = 5 * time.Second
	maxMessageBytes     = 64 * 1024
	sendBufferSize      = 64
)

// ServerConfig configures the WebSocket signaling Hub.
type ServerConfig struct {
	TURN         *TURNServer
	PingInterval time.Duration
	PongWait     time.Duration
	WriteWait    time.Duration
	Logger       *slog.Logger
}

// Server coordinates room membership and routes WebRTC signaling frames between peers.
type Server struct {
	turn         *TURNServer
	pingInterval time.Duration
	pongWait     time.Duration
	writeWait    time.Duration
	logger       *slog.Logger

	mu    sync.RWMutex
	room  *domain.Room
	peers map[string]*clientConn

	upgrader websocket.Upgrader
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type clientConn struct {
	peer      domain.PeerInfo
	conn      *websocket.Conn
	sendCh    chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (c *clientConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// NewServer initializes a WebSocket signaling Hub server.
func NewServer(cfg ServerConfig) *Server {
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = defaultPingInterval
	}
	pongWait := cfg.PongWait
	if pongWait <= 0 {
		pongWait = defaultPongWait
	}
	writeWait := cfg.WriteWait
	if writeWait <= 0 {
		writeWait = defaultWriteWait
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		turn:         cfg.TURN,
		pingInterval: pingInterval,
		pongWait:     pongWait,
		writeWait:    writeWait,
		logger:       logger,
		room:         domain.NewRoom(),
		peers:        make(map[string]*clientConn),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Handler returns the HTTP router exposing /ws and /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ws", s.handleWebSocket)
	return mux
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.ctx.Err() != nil {
		http.Error(w, "hub shutting down", http.StatusServiceUnavailable)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Debug("websocket upgrade failed", "err", err)
		return
	}

	conn.SetReadLimit(maxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(s.pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(s.pongWait))
	})

	_, rawJoin, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}

	joinEnv, err := domain.ParseEnvelope(rawJoin)
	if err != nil || joinEnv.Type != domain.MsgJoin {
		s.sendImmediateError(conn, "first signaling frame must be a valid join")
		_ = conn.Close()
		return
	}

	peer := domain.PeerInfo{
		ID:   joinEnv.SenderID,
		Name: joinEnv.SenderName,
	}

	client := &clientConn{
		peer:   peer,
		conn:   conn,
		sendCh: make(chan []byte, sendBufferSize),
		done:   make(chan struct{}),
	}

	if err := s.registerClient(client); err != nil {
		s.sendImmediateError(conn, err.Error())
		_ = conn.Close()
		return
	}

	s.wg.Add(2)
	go s.writePump(client)
	go s.readPump(client)
}

func (s *Server) registerClient(client *clientConn) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.room.Join(client.peer); err != nil {
		return err
	}
	s.peers[client.peer.ID] = client
	s.broadcastRosterLocked()
	return nil
}

func (s *Server) unregisterClient(client *clientConn) {
	s.mu.Lock()
	existing, ok := s.peers[client.peer.ID]
	if ok && existing == client {
		delete(s.peers, client.peer.ID)
		_ = s.room.Leave(client.peer.ID)
		s.broadcastRosterLocked()
	}
	s.mu.Unlock()
	client.close()
}

func (s *Server) broadcastRosterLocked() {
	rosterPeers := s.room.Peers()
	for peerID, target := range s.peers {
		env := domain.SignalEnvelope{
			Type:  domain.MsgRoster,
			Peers: rosterPeers,
		}
		if s.turn != nil {
			creds := s.turn.IssueCredentials(peerID)
			env.TURN = &creds
		}
		payload, err := env.Encode()
		if err != nil {
			s.logger.Error("encode roster failed", "err", err)
			continue
		}
		s.enqueue(target, payload)
	}
}

func (s *Server) enqueue(target *clientConn, payload []byte) {
	select {
	case <-target.done:
		return
	case <-s.ctx.Done():
		return
	default:
	}

	select {
	case target.sendCh <- payload:
	default:
		s.logger.Warn("closing slow peer", "peerId", target.peer.ID)
		target.close()
	}
}

func (s *Server) readPump(client *clientConn) {
	defer s.wg.Done()
	defer s.unregisterClient(client)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-client.done:
			return
		default:
		}

		_, data, err := client.conn.ReadMessage()
		if err != nil {
			return
		}

		env, err := domain.ParseEnvelope(data)
		if err != nil {
			s.sendErrorToClient(client, err.Error())
			continue
		}

		switch env.Type {
		case domain.MsgLeave:
			return
		case domain.MsgOffer, domain.MsgAnswer, domain.MsgCandidate:
			env.SenderID = client.peer.ID
			s.routeTargeted(client, env)
		default:
			s.sendErrorToClient(client, fmt.Sprintf("unsupported message type %q after join", env.Type))
		}
	}
}

func (s *Server) routeTargeted(sender *clientConn, env domain.SignalEnvelope) {
	payload, err := env.Encode()
	if err != nil {
		s.sendErrorToClient(sender, err.Error())
		return
	}

	s.mu.RLock()
	target, ok := s.peers[env.TargetID]
	s.mu.RUnlock()
	if !ok {
		s.sendErrorToClient(sender, fmt.Sprintf("target peer %q not found", env.TargetID))
		return
	}
	s.enqueue(target, payload)
}

func (s *Server) sendErrorToClient(client *clientConn, msg string) {
	payload, err := (domain.SignalEnvelope{
		Type:  domain.MsgError,
		Error: msg,
	}).Encode()
	if err != nil {
		return
	}
	s.enqueue(client, payload)
}

func (s *Server) sendImmediateError(conn *websocket.Conn, msg string) {
	payload, err := (domain.SignalEnvelope{
		Type:  domain.MsgError,
		Error: msg,
	}).Encode()
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(s.writeWait))
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}

func (s *Server) writePump(client *clientConn) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			_ = client.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "hub shutdown"), time.Now().Add(s.writeWait))
			return
		case <-client.done:
			return
		case payload := <-client.sendCh:
			_ = client.conn.SetWriteDeadline(time.Now().Add(s.writeWait))
			if err := client.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				client.close()
				return
			}
		case <-ticker.C:
			_ = client.conn.SetWriteDeadline(time.Now().Add(s.writeWait))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				client.close()
				return
			}
		}
	}
}

// Shutdown closes all connected signaling sockets and waits for active goroutines to exit.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()

	s.mu.RLock()
	clients := make([]*clientConn, 0, len(s.peers))
	for _, c := range s.peers {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	for _, c := range clients {
		c.close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("hub shutdown timed out waiting for connections")
	}
}
