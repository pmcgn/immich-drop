// Immich Drop Uploader – Backend (Go)
//
// Originally a rewrite of the Python/FastAPI backend (removed from the repo;
// docs/ preserves its behavior as the spec). Serves the static frontend,
// uploads to Immich using .env configuration only, performs duplicate checks
// (local SHA-1 cache + Immich bulk-check), and pushes per-session upload
// progress over WebSocket.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"immich-drop/internal/config"
	"immich-drop/internal/server"
	"immich-drop/internal/store"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=x.y.z"
//
// The Dockerfile passes it via the VERSION build arg; plain builds show "dev".
var version = "dev"

func logLevel(name string) slog.Level {
	switch name {
	case "DEBUG":
		return slog.LevelDebug
	case "WARNING", "WARN":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// healthProbe hits the running server's /healthz endpoint and returns a
// process exit code (0 = healthy). Used as the Docker HEALTHCHECK command:
// the distroless image has no shell or curl, so the binary probes itself.
// The endpoint lives on the admin port when split-port mode is active.
func healthProbe(cfg *config.Settings) int {
	port := cfg.Port
	if cfg.SplitPorts() {
		port = cfg.AdminPort
	}
	host := cfg.Host
	// Wildcard listen addresses are not dialable; probe via loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck failed: status", resp.StatusCode)
		return 1
	}
	return 0
}

func main() {
	healthFlag := flag.Bool("healthcheck", false,
		"probe the running server's health endpoint and exit (0 = healthy)")
	flag.Parse()

	cfg := config.Load()
	// Probe mode must not touch the SQLite database or start any listeners.
	if *healthFlag {
		os.Exit(healthProbe(cfg))
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(cfg.LogLevel),
	})))
	slog.Info("immich-drop version " + version + " started")

	st, err := store.Open(cfg.StateDB)
	if err != nil {
		slog.Error("failed to open state database", "path", cfg.StateDB, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Chunk spool directory (best-effort; chunk endpoints report errors if unusable).
	if err := os.MkdirAll(cfg.ChunkDir, 0o755); err != nil {
		slog.Warn("could not create chunk directory", "path", cfg.ChunkDir, "err", err)
	}

	srv := server.New(cfg, st)
	srv.StartChunkSweeper()
	addr := net.JoinHostPort(cfg.Host, cfg.Port)

	if !cfg.SplitPorts() {
		slog.Info("immich-drop (Go) listening", "addr", addr,
			"immich", cfg.NormalizedBaseURL(), "frontend", cfg.FrontendDir)
		if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
		return
	}

	// Split-port mode (ADMIN_PORT set): upload endpoints on PORT, admin
	// endpoints on ADMIN_PORT. Both listeners share the same Server (session
	// secret, SQLite store, album cache), so behavior is unchanged apart from
	// routing. Invite links built by the admin port fall back to the request
	// host when PUBLIC_BASE_URL is unset — that would point guests at the
	// admin port, hence the warning.
	adminAddr := net.JoinHostPort(cfg.Host, cfg.AdminPort)
	if cfg.PublicBaseURL == "" {
		slog.Warn("ADMIN_PORT is set but PUBLIC_BASE_URL is not; " +
			"invite links created on the admin port will point at the admin address")
	}
	slog.Info("immich-drop (Go) listening (split ports)",
		"upload_addr", addr, "admin_addr", adminAddr,
		"immich", cfg.NormalizedBaseURL(), "frontend", cfg.FrontendDir)

	errCh := make(chan error, 2)
	go func() { errCh <- http.ListenAndServe(addr, srv.UploadHandler()) }()
	go func() { errCh <- http.ListenAndServe(adminAddr, srv.AdminHandler()) }()
	slog.Error("server exited", "err", <-errCh)
	os.Exit(1)
}
