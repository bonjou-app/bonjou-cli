// Command bonjou-relay runs Bonjou Web's lightweight coordinator.
//
// It supplies source-network candidate groups, same-network rooms, and opaque
// encrypted WebRTC signaling. It never receives profiles, messages, file
// metadata, or file bytes, and it keeps no state across restarts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/relay"
	"github.com/bonjou-app/bonjou-cli/internal/version"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:46330",
		"address to listen on; bind to localhost and expose via a TLS reverse proxy")
	origins := flag.String("origins", "http://localhost:5173",
		"comma-separated browser origins allowed to use the coordinator, or * for any")
	logDir := flag.String("log-dir", defaultLogDir(), "directory for coordinator logs")
	trustProxy := flag.Bool("trust-proxy", true,
		"read the client address from X-Forwarded-For; only correct behind a trusted reverse proxy")
	clientIPHeader := flag.String("client-ip-header", "",
		"single client-IP header overwritten by the trusted ingress; requires -trust-proxy and rejects missing or invalid values")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		_, _ = fmt.Fprintf(os.Stdout, "bonjou-relay %s\n", version.Version)
		return
	}
	if strings.TrimSpace(*clientIPHeader) != "" && !*trustProxy {
		_, _ = fmt.Fprintln(os.Stderr, "bonjou-relay: -client-ip-header requires -trust-proxy")
		os.Exit(1)
	}

	lg, err := logger.New(*logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonjou-relay: open log dir %s: %v\n", *logDir, err)
		os.Exit(1)
	}
	defer func() {
		_ = lg.Close()
	}()

	srv := relay.NewServer(relay.Options{
		Limits:         relay.DefaultLimits(),
		Logger:         lg,
		AllowedOrigins: strings.Split(*origins, ","),
		TrustProxy:     *trustProxy,
		ClientIPHeader: *clientIPHeader,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Run(ctx)

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// WebSocket lifetime is managed by the connection ping loop.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	lg.Info("coordinator: listening on %s (origins=%s, trust-proxy=%t)", *addr, *origins, *trustProxy)
	_, _ = fmt.Fprintf(os.Stdout, "bonjou-relay %s listening on %s\n", version.Version, *addr)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		lg.Error("coordinator: listen failed: %v", err)
		fmt.Fprintf(os.Stderr, "bonjou-relay: %v\n", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	lg.Info("coordinator: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		lg.Error("coordinator: shutdown: %v", err)
	}
}

func defaultLogDir() string {
	if dir := os.Getenv("BONJOU_RELAY_LOG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bonjou-relay")
	}
	return filepath.Join(home, ".bonjou", "relay-logs")
}
