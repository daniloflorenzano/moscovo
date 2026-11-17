package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"moscovo/internal/hub"
)

func main() {
	var (
		listenIP = flag.String("ip", "", "IP address to bind signaling and TURN listeners (auto-detects tailscale0 if empty)")
		relayIP  = flag.String("relay-ip", "", "External/Tailscale IP advertised by TURN relay (defaults to -ip)")
		wsPort   = flag.Int("ws-port", 7878, "WebSocket signaling listen port")
		turnPort = flag.Int("turn-port", 3478, "Embedded STUN/TURN UDP listen port")
		realm    = flag.String("realm", "moscovo.hub", "TURN authentication realm")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ip := *listenIP
	if ip == "" {
		ip = detectTailscaleIPv4()
	}

	turnSrv, err := hub.NewTURNServer(hub.TURNConfig{
		ListenIP: ip,
		RelayIP:  *relayIP,
		Port:     *turnPort,
		Realm:    *realm,
	})
	if err != nil {
		logger.Error("failed to start embedded STUN/TURN server", "err", err)
		os.Exit(1)
	}

	hubSrv := hub.NewServer(hub.ServerConfig{
		TURN:   turnSrv,
		Logger: logger,
	})

	addr := fmt.Sprintf("%s:%d", ip, *wsPort)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           hubSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("moscovo-hub listening", "ws", "ws://"+addr+"/ws", "turnPort", turnSrv.Port())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down moscovo-hub")
	case err := <-errCh:
		if err != nil {
			logger.Error("http server error", "err", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", "err", err)
	}
	if err := hubSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("hub shutdown error", "err", err)
	}
	if err := turnSrv.Close(); err != nil {
		logger.Warn("turn close error", "err", err)
	}
	logger.Info("moscovo-hub stopped cleanly")
}

func detectTailscaleIPv4() string {
	iface, err := net.InterfaceByName("tailscale0")
	if err == nil {
		addrs, addrErr := iface.Addrs()
		if addrErr == nil {
			for _, a := range addrs {
				ipNet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if ipv4 := ipNet.IP.To4(); ipv4 != nil {
					return ipv4.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
