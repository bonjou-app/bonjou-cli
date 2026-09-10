package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/commands"
	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/history"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/network"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
	"github.com/bonjou-app/bonjou-cli/internal/session"
	"github.com/bonjou-app/bonjou-cli/internal/ui"
	"github.com/bonjou-app/bonjou-cli/internal/version"
)

func main() {
	args := os.Args[1:]
	if isVersionQuery(args) {
		fmt.Println(version.Version)
		return
	}
	if !invocationIsCanonical() {
		fmt.Fprintln(os.Stderr, "Launch Bonjou using the lowercase `bonjou` command.")
		os.Exit(1)
	}
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "bonjou does not accept arguments. Run `bonjou --version` to check the version.")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare directories: %v\n", err)
		os.Exit(1)
	}
	log, err := logger.New(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	hist := history.New(cfg)
	eventStream := make(chan events.Event, 128)

	ip, err := config.GetLocalIP()
	if err != nil {
		ip = "127.0.0.1"
	}

	discovery := network.NewDiscoveryService(cfg, log)
	knownPeers, err := network.NewKnownPeers(filepath.Join(cfg.BaseDir, "known_peers.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load known-peers store: %v\n", err)
		os.Exit(1)
	}
	discovery.SetKnownPeers(knownPeers)
	queueMgr, err := queue.NewManager(cfg.BaseDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise approval queue: %v\n", err)
		os.Exit(1)
	}

	transfer := network.NewTransferService(cfg, log, hist, eventStream, discovery, queueMgr)
	if err := transfer.Start(cfg.Username, ip); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start transfer service: %v\n", err)
		os.Exit(1)
	}

	if err := discovery.Start(cfg.Username, ip, cfg.ListenPort); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start discovery service: %v\n", err)
		os.Exit(1)
	}

	sess := session.New(cfg, log, hist, discovery, transfer, eventStream, ip, queueMgr)
	stopWatcher := sess.StartNetworkWatcher(5 * time.Second)
	handler := commands.New(sess)
	console, err := ui.New(sess, handler)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise console: %v\n", err)
		sess.Close()
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	notifySignals(sigs)
	go func() {
		for range sigs {
			if sess.Transfer != nil && sess.Transfer.CancelActiveOperation() {
				fmt.Fprintln(os.Stderr, "\nInterrupted active transfer work. Press Ctrl+C again to exit Bonjou.")
				continue
			}
			fmt.Fprintln(os.Stderr, "\nSignal received, shutting down...")
			stopWatcher()
			sess.Close()
			os.Exit(0)
		}
	}()

	console.Run()
	stopWatcher()
	sess.Close()
}

func invocationIsCanonical() bool {
	base := filepath.Base(os.Args[0])
	switch runtime.GOOS {
	case "windows":
		return base == "bonjou.exe"
	default:
		return base == "bonjou"
	}
}

func isVersionQuery(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--version", "-v", "-V":
		return true
	default:
		return false
	}
}
