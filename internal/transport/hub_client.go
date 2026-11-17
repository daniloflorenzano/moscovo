package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"moscovo/internal/domain"
)

const (
	defaultDialTimeout      = 5 * time.Second
	defaultClientWriteWait  = 5 * time.Second
	defaultClientPongWait   = 25 * time.Second
	defaultClientPingPeriod = 10 * time.Second
	leaveTimeout            = 500 * time.Millisecond
	initialReconnectBackoff = 500 * time.Millisecond
	maxReconnectBackoff     = 10 * time.Second
)

// HubConnectionState represents the client's connection status with the Shared Hub.
type HubConnectionState string

const (
	HubOffline      HubConnectionState = "OFFLINE"
	HubConnecting   HubConnectionState = "CONNECTING"
	HubOnline       HubConnectionState = "ONLINE"
	HubReconnecting HubConnectionState = "RECONNECTING"
)

// HubClientConfig configures the WebSocket signaling client connected to the Shared Hub.
type HubClientConfig struct {
	HubURL        string
	Peer          domain.PeerInfo
	DialTimeout   time.Duration
	WriteTimeout  time.Duration
	PongWait      time.Duration
	PingInterval  time.Duration
	OnEnvelope    func(domain.SignalEnvelope)
	OnStateChange func(HubConnectionState, error)
	Logger        *slog.Logger
}

// HubClient maintains a context-bounded WebSocket connection to moscovo-hub.
type HubClient struct {
	wsURL        string
	peer         domain.PeerInfo
	dialTimeout  time.Duration
	writeTimeout time.Duration
	pongWait     time.Duration
	pingInterval time.Duration
	onEnvelope   func(domain.SignalEnvelope)
	onState      func(HubConnectionState, error)
	logger       *slog.Logger

	mu          sync.Mutex
	conn        *websocket.Conn
	sessionDone chan struct{}
	closed      bool
	closeCh     chan struct{}
	closeOnce   sync.Once

	writeMu sync.Mutex
	wg      sync.WaitGroup
}

// NormalizeHubWSURL converts a host:port, http(s)://, or ws(s):// Hub address into a /ws WebSocket URL.
func NormalizeHubWSURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("hub address cannot be empty")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "ws://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse hub address %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported hub scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("hub address %q missing host", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws"
	}
	return u.String(), nil
}

// NewHubClient constructs a HubClient ready to connect to the Shared Hub.
func NewHubClient(cfg HubClientConfig) (*HubClient, error) {
	wsURL, err := NormalizeHubWSURL(cfg.HubURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Peer.ID) == "" || strings.TrimSpace(cfg.Peer.Name) == "" {
		return nil, errors.New("peer id and name cannot be empty")
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = defaultDialTimeout
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultClientWriteWait
	}
	pongWait := cfg.PongWait
	if pongWait <= 0 {
		pongWait = defaultClientPongWait
	}
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = defaultClientPingPeriod
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &HubClient{
		wsURL:        wsURL,
		peer:         cfg.Peer,
		dialTimeout:  dialTimeout,
		writeTimeout: writeTimeout,
		pongWait:     pongWait,
		pingInterval: pingInterval,
		onEnvelope:   cfg.OnEnvelope,
		onState:      cfg.OnStateChange,
		logger:       logger,
		closeCh:      make(chan struct{}),
	}, nil
}

// Connect dials the Hub WebSocket, sends the initial join frame, and starts read/ping loops.
func (c *HubClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("hub client closed")
	}
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	c.notifyState(HubConnecting, nil)
	c.logger.Info("connecting to hub", "url", c.wsURL, "peerId", c.peer.ID)

	dialCtx, cancel := context.WithTimeout(ctx, c.dialTimeout)
	defer cancel()

	dialer := websocket.Dialer{
		HandshakeTimeout: c.dialTimeout,
	}
	conn, _, err := dialer.DialContext(dialCtx, c.wsURL, nil)
	if err != nil {
		c.logger.Warn("hub connection failed", "url", c.wsURL, "err", err)
		c.notifyState(HubOffline, err)
		return fmt.Errorf("dial hub %s: %w", c.wsURL, err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(c.pongWait))
	})
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(c.writeTimeout))
	})

	sessionDone := make(chan struct{})
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return errors.New("hub client closed")
	}
	c.conn = conn
	c.sessionDone = sessionDone
	c.mu.Unlock()

	joinEnv := domain.SignalEnvelope{
		Type:       domain.MsgJoin,
		SenderID:   c.peer.ID,
		SenderName: c.peer.Name,
	}
	if err := c.Send(ctx, joinEnv); err != nil {
		c.logger.Warn("failed to send hub join frame", "url", c.wsURL, "err", err)
		c.dropSession(conn, sessionDone)
		c.notifyState(HubOffline, err)
		return fmt.Errorf("send join: %w", err)
	}

	c.logger.Info("connected to hub", "url", c.wsURL, "peerId", c.peer.ID)
	c.notifyState(HubOnline, nil)

	c.wg.Add(2)
	go c.readLoop(conn, sessionDone)
	go c.pingLoop(conn, sessionDone)
	return nil
}

// RunWithReconnect maintains the signaling session until ctx is cancelled or LeaveAndClose is called.
func (c *HubClient) RunWithReconnect(ctx context.Context) {
	backoff := initialReconnectBackoff
	for {
		if ctx.Err() != nil || c.isClosed() {
			return
		}

		err := c.Connect(ctx)
		if err == nil {
			backoff = initialReconnectBackoff
			c.mu.Lock()
			done := c.sessionDone
			c.mu.Unlock()

			if done != nil {
				select {
				case <-ctx.Done():
					return
				case <-c.closeCh:
					return
				case <-done:
				}
			}
			if ctx.Err() != nil || c.isClosed() {
				return
			}
			c.notifyState(HubReconnecting, errors.New("hub connection lost"))
		} else {
			if ctx.Err() != nil || c.isClosed() {
				return
			}
			c.notifyState(HubReconnecting, err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.closeCh:
			timer.Stop()
			return
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxReconnectBackoff {
			backoff = maxReconnectBackoff
		}
	}
}

// Send validates, encodes, and writes a signaling envelope with a context-bounded deadline.
func (c *HubClient) Send(ctx context.Context, env domain.SignalEnvelope) error {
	payload, err := env.Encode()
	if err != nil {
		return err
	}

	c.mu.Lock()
	conn := c.conn
	closed := c.closed
	c.mu.Unlock()
	if closed || conn == nil {
		return errors.New("hub client not connected")
	}

	deadline := time.Now().Add(c.writeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// LeaveAndClose politely sends a leave frame (500ms timeout), closes the socket, and waits for goroutines.
func (c *HubClient) LeaveAndClose() error {
	var leaveErr error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()

		close(c.closeCh)

		if conn != nil {
			leaveCtx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
			payload, encErr := (domain.SignalEnvelope{
				Type:     domain.MsgLeave,
				SenderID: c.peer.ID,
			}).Encode()
			if encErr == nil {
				deadline, _ := leaveCtx.Deadline()
				c.writeMu.Lock()
				_ = conn.SetWriteDeadline(deadline)
				leaveErr = conn.WriteMessage(websocket.TextMessage, payload)
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
					deadline,
				)
				c.writeMu.Unlock()
			}
			cancel()
			_ = conn.Close()
		}
	})

	c.wg.Wait()
	c.notifyState(HubOffline, nil)
	return leaveErr
}

func (c *HubClient) readLoop(conn *websocket.Conn, sessionDone chan struct{}) {
	defer c.wg.Done()
	defer c.dropSession(conn, sessionDone)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))

		env, err := domain.ParseEnvelope(data)
		if err != nil {
			c.logger.Warn("ignoring invalid hub envelope", "err", err)
			continue
		}
		if c.onEnvelope != nil {
			c.onEnvelope(env)
		}
	}
}

func (c *HubClient) pingLoop(conn *websocket.Conn, sessionDone chan struct{}) {
	defer c.wg.Done()
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sessionDone:
			return
		case <-c.closeCh:
			return
		case <-ticker.C:
			c.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.writeTimeout))
			c.writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (c *HubClient) dropSession(conn *websocket.Conn, sessionDone chan struct{}) {
	_ = conn.Close()
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()

	select {
	case <-sessionDone:
	default:
		close(sessionDone)
	}
}

func (c *HubClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *HubClient) notifyState(state HubConnectionState, err error) {
	if c.onState != nil {
		c.onState(state, err)
	}
}
