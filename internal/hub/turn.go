package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"

	"moscovo/internal/domain"
)

const (
	defaultTURNRealm = "moscovo.hub"
	defaultTURNTTL   = 12 * time.Hour
)

// TURNConfig configures the embedded STUN/TURN server on the Shared Hub.
type TURNConfig struct {
	ListenIP string
	RelayIP  string
	Port     int
	Realm    string
	Secret   []byte
	TTL      time.Duration
}

// TURNServer wraps an embedded pion/turn server and issues short-lived HMAC credentials.
type TURNServer struct {
	server  *turn.Server
	realm   string
	relayIP string
	port    int
	secret  []byte
	ttl     time.Duration
}

// NewTURNServer binds a UDP STUN/TURN listener and starts serving relay allocations.
func NewTURNServer(cfg TURNConfig) (*TURNServer, error) {
	if strings.TrimSpace(cfg.ListenIP) == "" {
		return nil, errors.New("turn listen ip cannot be empty")
	}
	relayIP := strings.TrimSpace(cfg.RelayIP)
	if relayIP == "" {
		relayIP = cfg.ListenIP
	}
	parsedRelayIP := net.ParseIP(relayIP)
	if parsedRelayIP == nil {
		return nil, fmt.Errorf("invalid turn relay ip %q", relayIP)
	}
	realm := strings.TrimSpace(cfg.Realm)
	if realm == "" {
		realm = defaultTURNRealm
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultTURNTTL
	}

	secret := cfg.Secret
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate turn secret: %w", err)
		}
	}

	addr := net.JoinHostPort(cfg.ListenIP, strconv.Itoa(cfg.Port))
	pktConn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen turn udp %s: %w", addr, err)
	}

	udpAddr, ok := pktConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = pktConn.Close()
		return nil, errors.New("turn packet conn did not yield a UDPAddr")
	}

	ts := &TURNServer{
		realm:   realm,
		relayIP: relayIP,
		port:    udpAddr.Port,
		secret:  append([]byte(nil), secret...),
		ttl:     ttl,
	}

	loggerFactory := logging.NewDefaultLoggerFactory()
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		AuthHandler:   ts.authenticate,
		LoggerFactory: loggerFactory,
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: pktConn,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: parsedRelayIP,
					Address:      cfg.ListenIP,
				},
			},
		},
	})
	if err != nil {
		_ = pktConn.Close()
		return nil, fmt.Errorf("create pion turn server: %w", err)
	}
	ts.server = srv
	return ts, nil
}

// Port returns the bound UDP port of the STUN/TURN listener.
func (s *TURNServer) Port() int {
	return s.port
}

// IssueCredentials mints short-lived HMAC-signed TURN credentials for peerID.
func (s *TURNServer) IssueCredentials(peerID string) domain.TURNCredentials {
	expiresAt := time.Now().Add(s.ttl).Unix()
	username := fmt.Sprintf("%d:%s", expiresAt, peerID)
	password := s.signUsername(username)
	return domain.TURNCredentials{
		URLs:      []string{fmt.Sprintf("turn:%s:%d?transport=udp", s.relayIP, s.port)},
		Username:  username,
		Password:  password,
		ExpiresAt: expiresAt,
	}
}

func (s *TURNServer) signUsername(username string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (s *TURNServer) authenticate(ra *turn.RequestAttributes) (string, []byte, bool) {
	if ra == nil || ra.Username == "" {
		return "", nil, false
	}
	parts := strings.SplitN(ra.Username, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", nil, false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return "", nil, false
	}
	password := s.signUsername(ra.Username)
	return parts[1], turn.GenerateAuthKey(ra.Username, s.realm, password), true
}

// Close shuts down the embedded TURN server and its UDP socket.
func (s *TURNServer) Close() error {
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}
