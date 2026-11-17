package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"moscovo/frontend"
	"moscovo/internal/app"
)

func main() {
	var (
		hubAddr    = flag.String("hub", "127.0.0.1:7878", "Shared Tailscale Hub address (host:port or ws:// URL)")
		nickname   = flag.String("nick", "", "Display nickname in the voice room")
		forceRelay = flag.Bool("force-relay", false, "Force TURN relay transport policy")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	voiceSvc := app.NewVoiceService(app.ServiceConfig{
		HubAddr:    *hubAddr,
		Nickname:   *nickname,
		ForceRelay: *forceRelay,
		Logger:     logger,
	})

	wailsApp := application.New(application.Options{
		Name:        "Moscovo",
		Description: "Minimalist P2P voice communication via Shared Tailscale Hub",
		Logger:      logger,
		Services: []application.Service{
			application.NewServiceWithOptions(voiceSvc, application.ServiceOptions{
				Name:  "VoiceService",
				Route: "/api",
			}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(frontend.Assets),
			Middleware: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, ".ts") {
						w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
					}
					next.ServeHTTP(w, r)
				})
			},
		},
		OnShutdown: func() {
			if err := voiceSvc.Shutdown(); err != nil {
				logger.Warn("voice service shutdown error", "err", err)
			}
		},
	})

	voiceSvc.SetEmitter(func(name string, data any) {
		wailsApp.Event.Emit(name, data)
	})

	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Moscovo",
		Width:            360,
		Height:           480,
		MinWidth:         280,
		MinHeight:        320,
		BackgroundColour: application.NewRGBA(12, 15, 20, 255),
		URL:              "/",
	})

	if err := wailsApp.Run(); err != nil {
		logger.Error("moscovo desktop app exited with error", "err", err)
		os.Exit(1)
	}
}
