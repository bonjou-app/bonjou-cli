package network

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/history"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
)

func TestDirectorySizeCountsNestedFiles(t *testing.T) {
	root := t.TempDir()

	writeSizedFile(t, filepath.Join(root, "a.txt"), 11)
	writeSizedFile(t, filepath.Join(root, "nested", "b.bin"), 23)
	writeSizedFile(t, filepath.Join(root, "nested", "deeper", "c.dat"), 7)

	size, err := directorySize(root, nil)
	if err != nil {
		t.Fatalf("directorySize returned error: %v", err)
	}

	const want int64 = 11 + 23 + 7
	if size != want {
		t.Fatalf("directorySize = %d, want %d", size, want)
	}
}

func TestDirectorySizeIgnoresDirectories(t *testing.T) {
	root := t.TempDir()

	for _, dir := range []string{
		filepath.Join(root, "empty"),
		filepath.Join(root, "nested", "also-empty"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	size, err := directorySize(root, nil)
	if err != nil {
		t.Fatalf("directorySize returned error: %v", err)
	}
	if size != 0 {
		t.Fatalf("directorySize = %d, want 0", size)
	}
}

func TestMetadataQueuePersistsFolderPreviewAndSize(t *testing.T) {
	baseDir := t.TempDir()

	mgr, err := queue.NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	const (
		requestID = "req-folder-123"
		sender    = "hamza"
		senderIP  = "192.168.1.3"
		name      = "q-chess"
		size      = int64(194_200_000)
		preview   = "src/\nREADME.md\nassets/\n... and 12 more entries"
	)

	id, err := mgr.AddFolder(requestID, sender, senderIP, name, size, preview)
	if err != nil {
		t.Fatalf("AddFolder returned error: %v", err)
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reloaded, err := queue.NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager reload returned error: %v", err)
	}

	folder, err := reloaded.GetFolder(id)
	if err != nil {
		t.Fatalf("GetFolder returned error: %v", err)
	}

	if folder.RequestID != requestID {
		t.Fatalf("RequestID = %q, want %q", folder.RequestID, requestID)
	}
	if folder.Sender != sender {
		t.Fatalf("Sender = %q, want %q", folder.Sender, sender)
	}
	if folder.SenderIP != senderIP {
		t.Fatalf("SenderIP = %q, want %q", folder.SenderIP, senderIP)
	}
	if folder.Name != name {
		t.Fatalf("Name = %q, want %q", folder.Name, name)
	}
	if folder.Size != size {
		t.Fatalf("Size = %d, want %d", folder.Size, size)
	}
	if folder.Preview != preview {
		t.Fatalf("Preview = %q, want %q", folder.Preview, preview)
	}
}

func TestMetadataFirstFileOfferApproveRoundTrip(t *testing.T) {
	sender, receiver := newTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "hello.txt")
	writeFileString(t, payloadPath, "metadata-first file transfer")

	receiverPeer := peerForHarness(t, receiver)

	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	fileID := waitForPendingFileCount(t, receiver.queue, 1)
	pending, err := receiver.queue.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile returned error: %v", err)
	}

	gotQueueSize := pending.Size
	if info, err := os.Stat(payloadPath); err != nil {
		t.Fatalf("stat payload: %v", err)
	} else if gotQueueSize != info.Size() {
		t.Fatalf("pending.Size = %d, want %d", gotQueueSize, info.Size())
	}

	if entries, err := os.ReadDir(receiver.cfg.ReceivedFilesDir); err != nil {
		t.Fatalf("read received files dir before approval: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received files dir not empty before approval: %d entries", len(entries))
	}

	destPath := filepath.Join(receiver.cfg.ReceivedFilesDir, pending.Name)
	if err := receiver.transfer.ApproveFileTransfer(pending, destPath); err != nil {
		t.Fatalf("ApproveFileTransfer returned error: %v", err)
	}
	if err := receiver.queue.RemoveFile(fileID); err != nil {
		t.Fatalf("RemoveFile returned error: %v", err)
	}

	waitForFileContent(t, destPath, "metadata-first file transfer")
	waitForNoPendingFiles(t, receiver.queue)
	waitForNoOutgoingApproval(t, sender.transfer, pending.RequestID)
}

func TestMetadataFirstFolderOfferRejectRoundTrip(t *testing.T) {
	sender, receiver := newTransferPair(t)

	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "project")
	writeSizedFile(t, filepath.Join(folderPath, "README.md"), 12)
	writeSizedFile(t, filepath.Join(folderPath, "nested", "main.go"), 37)

	receiverPeer := peerForHarness(t, receiver)

	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder returned error: %v", err)
	}

	folderID := waitForPendingFolderCount(t, receiver.queue, 1)
	pending, err := receiver.queue.GetFolder(folderID)
	if err != nil {
		t.Fatalf("GetFolder returned error: %v", err)
	}

	wantSize, err := directorySize(folderPath, nil)
	if err != nil {
		t.Fatalf("directorySize returned error: %v", err)
	}
	if pending.Size != wantSize {
		t.Fatalf("pending.Size = %d, want %d", pending.Size, wantSize)
	}
	if pending.Preview == "" {
		t.Fatal("pending.Preview should not be empty")
	}

	if err := receiver.transfer.RejectFolderTransfer(pending); err != nil {
		t.Fatalf("RejectFolderTransfer returned error: %v", err)
	}
	if err := receiver.queue.RemoveFolder(folderID); err != nil {
		t.Fatalf("RemoveFolder returned error: %v", err)
	}

	waitForNoPendingFolders(t, receiver.queue)
	waitForNoOutgoingApproval(t, sender.transfer, pending.RequestID)

	if entries, err := os.ReadDir(receiver.cfg.ReceivedFoldersDir); err != nil {
		t.Fatalf("read received folders dir after rejection: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received folders dir not empty after rejection: %d entries", len(entries))
	}
}

type transferHarness struct {
	cfg       *config.Config
	log       *logger.Logger
	queue     *queue.Manager
	discovery *DiscoveryService
	transfer  *TransferService
}

func newTransferPair(t *testing.T) (*transferHarness, *transferHarness) {
	t.Helper()

	sender := newTransferHarness(t, "sender", "127.0.0.1", 0, 47320, "shared-secret-for-tests")
	receiver := newTransferHarness(t, "receiver", "127.0.0.1", 0, 47320, "shared-secret-for-tests")

	// Register cleanup before startup so a failed second listener cannot leak
	// the first listener into later tests.
	t.Cleanup(func() {
		sender.transfer.Stop()
		receiver.transfer.Stop()
		_ = sender.queue.Close()
		_ = receiver.queue.Close()
		_ = sender.log.Close()
		_ = receiver.log.Close()
	})

	startTransferHarness(t, sender)
	startTransferHarness(t, receiver)
	linkPeers(t, sender, receiver)
	linkPeers(t, receiver, sender)
	return sender, receiver
}

func startTransferHarness(t *testing.T, h *transferHarness) {
	t.Helper()

	// Probing a free port releases it before Start can bind. Another test or
	// an outgoing connection can claim it in that gap, so retry a fresh port.
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		h.cfg.ListenPort = reserveTCPPort(t)
		if err = h.transfer.Start(h.cfg.Username, "127.0.0.1"); err == nil {
			return
		}
	}
	t.Fatalf("start %s transfer after 10 port attempts: %v", h.cfg.Username, err)
}

func newTransferHarness(t *testing.T, username, ip string, listenPort, discoveryPort int, secret string) *transferHarness {
	t.Helper()

	baseDir := t.TempDir()
	cfg := &config.Config{
		Username:           username,
		ListenPort:         listenPort,
		DiscoveryPort:      discoveryPort,
		BaseDir:            baseDir,
		SaveDir:            filepath.Join(baseDir, "received"),
		LogDir:             filepath.Join(baseDir, "logs"),
		ReceivedFilesDir:   filepath.Join(baseDir, "received", "files"),
		ReceivedFoldersDir: filepath.Join(baseDir, "received", "folders"),
		Secret:             secret,
		ChunkSize:          8 * 1024,
		ChunkTimeoutSecs:   30,
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("EnsureDirectories returned error: %v", err)
	}

	log, err := logger.New(cfg.LogDir)
	if err != nil {
		t.Fatalf("logger.New returned error: %v", err)
	}

	queueMgr, err := queue.NewManager(cfg.BaseDir)
	if err != nil {
		t.Fatalf("queue.NewManager returned error: %v", err)
	}

	eventsCh := make(chan events.Event, 1024)
	hist := history.New(cfg)
	discovery := NewDiscoveryService(cfg, log)
	transfer := NewTransferService(cfg, log, hist, eventsCh, discovery, queueMgr)

	return &transferHarness{
		cfg:       cfg,
		log:       log,
		queue:     queueMgr,
		discovery: discovery,
		transfer:  transfer,
	}
}

func linkPeers(t *testing.T, local, remote *transferHarness) {
	t.Helper()

	pub, err := localPublicKeyFromSecret(remote.cfg.Secret)
	if err != nil {
		t.Fatalf("localPublicKeyFromSecret returned error: %v", err)
	}

	local.discovery.mu.Lock()
	local.discovery.peers[remoteIPForTest(remote)] = &Peer{
		Username:  remote.cfg.Username,
		IP:        remoteIPForTest(remote),
		Port:      remote.cfg.ListenPort,
		LastSeen:  time.Now(),
		PublicKey: pub,
	}
	local.discovery.mu.Unlock()
}

func peerForHarness(t *testing.T, h *transferHarness) *Peer {
	t.Helper()

	pub, err := localPublicKeyFromSecret(h.cfg.Secret)
	if err != nil {
		t.Fatalf("localPublicKeyFromSecret returned error: %v", err)
	}

	return &Peer{
		Username:  h.cfg.Username,
		IP:        "127.0.0.1",
		Port:      h.cfg.ListenPort,
		PublicKey: pub,
	}
}

func remoteIPForTest(h *transferHarness) string {
	_ = h
	return "127.0.0.1"
}

func waitForPendingFileCount(t *testing.T, mgr *queue.Manager, want int) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files := mgr.ListFiles()
		if len(files) == want {
			return files[0].ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending file(s)", want)
	return 0
}

func waitForPendingFolderCount(t *testing.T, mgr *queue.Manager, want int) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		folders := mgr.ListFolders()
		if len(folders) == want {
			return folders[0].ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending folder(s)", want)
	return 0
}

func waitForNoPendingFiles(t *testing.T, mgr *queue.Manager) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.ListFiles()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for no pending files")
}

func waitForNoPendingFolders(t *testing.T, mgr *queue.Manager) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.ListFolders()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for no pending folders")
}

func waitForNoOutgoingApproval(t *testing.T, transfer *TransferService, requestID string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := transfer.getOutgoingApproval(requestID); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for outgoing approval %q to be cleared", requestID)
}

func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		var err error
		data, err = os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read file %s: %v", path, err)
		}
		// File creation precedes the asynchronous payload write.
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for file %s: content = %q, want %q", path, string(data), want)
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}

	data := make([]byte, size)
	for i := range data {
		data[i] = byte('a' + (i % 26))
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func writeFileString(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserveTCPPort listen failed: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("reserveTCPPort unexpected addr type: %T", ln.Addr())
	}
	return addr.Port
}
