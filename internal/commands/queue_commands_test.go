package commands

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/history"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/network"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
	"github.com/bonjou-app/bonjou-cli/internal/session"
)

func TestPendingMetadataOfferSurvivesRestartAndCanBeRejectedViaHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "restart.txt")
	writeFileStringForCommands(t, payloadPath, "restart recovery payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	fileID := waitForPendingFileCountForCommands(t, receiver.queue, 1)
	pendingBeforeRestart, err := receiver.queue.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile before restart returned error: %v", err)
	}

	receiver.transfer.Stop()
	_ = receiver.queue.Close()
	_ = receiver.log.Close()

	restarted := restartReceiverHarness(t, receiver, sender)

	handler := New(restarted.session)

	viewResult, err := handler.Handle("@view 1")
	if err != nil {
		t.Fatalf("Handle(@view 1) returned error: %v", err)
	}
	if !strings.Contains(viewResult.Output, pendingBeforeRestart.Name) {
		t.Fatalf("view output missing pending item name: %q", viewResult.Output)
	}
	if !strings.Contains(viewResult.Output, "Status: not downloaded yet") {
		t.Fatalf("view output missing metadata-first status: %q", viewResult.Output)
	}

	rejectResult, err := handler.Handle("@reject 1")
	if err != nil {
		t.Fatalf("Handle(@reject 1) returned error: %v", err)
	}
	if !strings.Contains(rejectResult.Output, "Rejected file") {
		t.Fatalf("reject output = %q, want rejection confirmation", rejectResult.Output)
	}

	waitForNoPendingFilesForCommands(t, restarted.queue)
	waitForNoOutgoingApprovalForCommands(t, sender.transfer, pendingBeforeRestart.RequestID)

	if entries, err := os.ReadDir(restarted.cfg.ReceivedFilesDir); err != nil {
		t.Fatalf("ReadDir(received files) returned error: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received files dir should stay empty after rejection, got %d entries", len(entries))
	}
}

func TestPendingMetadataOfferSurvivesRestartAndCanBeApprovedViaHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "restart-approve.txt")
	writeFileStringForCommands(t, payloadPath, "restart approval payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	fileID := waitForPendingFileCountForCommands(t, receiver.queue, 1)
	pendingBeforeRestart, err := receiver.queue.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile before restart returned error: %v", err)
	}

	receiver.transfer.Stop()
	_ = receiver.queue.Close()
	_ = receiver.log.Close()

	restarted := restartReceiverHarness(t, receiver, sender)
	handler := New(restarted.session)

	result, err := handler.Handle("@approve 1")
	if err != nil {
		t.Fatalf("Handle(@approve 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Approved file") {
		t.Fatalf("approve output = %q, want approval confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, restarted.queue)
	waitForNoOutgoingApprovalForCommands(t, sender.transfer, pendingBeforeRestart.RequestID)
	waitForFileContentForCommands(t, filepath.Join(restarted.cfg.ReceivedFilesDir, pendingBeforeRestart.Name), "restart approval payload")
}

func TestViewFileOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "view-file.txt")
	writeFileStringForCommands(t, payloadPath, "view file payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@view 1")
	if err != nil {
		t.Fatalf("Handle(@view 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Pending file [1]") {
		t.Fatalf("view output = %q, want file heading", result.Output)
	}
	if !strings.Contains(result.Output, "Status: not downloaded yet") {
		t.Fatalf("view output = %q, want metadata-first status", result.Output)
	}
}

func TestSenderRestartBeforeReceiverApprovalStillAllowsApproveViaHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "sender-restart.txt")
	writeFileStringForCommands(t, payloadPath, "sender restart payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	fileID := waitForPendingFileCountForCommands(t, receiver.queue, 1)
	pending, err := receiver.queue.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile returned error: %v", err)
	}

	sender.transfer.Stop()
	_ = sender.queue.Close()
	_ = sender.log.Close()

	restartedSender := restartSenderHarness(t, sender, receiver)

	handler := New(receiver.session)
	result, err := handler.Handle("@approve 1")
	if err != nil {
		t.Fatalf("Handle(@approve 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Approved file") {
		t.Fatalf("approve output = %q, want approval confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForNoOutgoingApprovalForCommands(t, restartedSender.transfer, pending.RequestID)
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFilesDir, pending.Name), "sender restart payload")
}

func TestViewFolderOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "view-folder")
	writeFileStringForCommands(t, filepath.Join(folderPath, "README.md"), "folder readme")
	writeFileStringForCommands(t, filepath.Join(folderPath, "nested", "main.go"), "package main\n")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder returned error: %v", err)
	}

	waitForPendingFolderCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@view 1")
	if err != nil {
		t.Fatalf("Handle(@view 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Pending Folder [1]") {
		t.Fatalf("view output = %q, want folder heading", result.Output)
	}
	if !strings.Contains(result.Output, "Manifest:") {
		t.Fatalf("view output = %q, want manifest heading", result.Output)
	}
	if !strings.Contains(result.Output, "README.md") {
		t.Fatalf("view output = %q, want manifest entry", result.Output)
	}
}

func TestApproveSingleFileOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "approve-single.txt")
	writeFileStringForCommands(t, payloadPath, "approve single payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@approve 1")
	if err != nil {
		t.Fatalf("Handle(@approve 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Approved file") {
		t.Fatalf("approve output = %q, want approval confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFilesDir, "approve-single.txt"), "approve single payload")
	// Receiving the bytes can finish before the sender processes the final ack.
	for requestID := range sender.transferPendingApprovals() {
		waitForNoOutgoingApprovalForCommands(t, sender.transfer, requestID)
	}
}

func TestApproveSingleFolderOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "approve-folder")
	writeFileStringForCommands(t, filepath.Join(folderPath, "README.md"), "approve folder readme")
	writeFileStringForCommands(t, filepath.Join(folderPath, "nested", "main.go"), "package main\n")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder returned error: %v", err)
	}

	waitForPendingFolderCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@approve 1")
	if err != nil {
		t.Fatalf("Handle(@approve 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Approved folder") {
		t.Fatalf("approve output = %q, want approval confirmation", result.Output)
	}

	waitForNoPendingFoldersForCommands(t, receiver.queue)
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFoldersDir, "approve-folder", "README.md"), "approve folder readme")
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFoldersDir, "approve-folder", "nested", "main.go"), "package main\n")
	for requestID := range sender.transferPendingApprovals() {
		waitForNoOutgoingApprovalForCommands(t, sender.transfer, requestID)
	}
}

func TestRejectSingleFileOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "reject-file.txt")
	writeFileStringForCommands(t, payloadPath, "reject file payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	fileID := waitForPendingFileCountForCommands(t, receiver.queue, 1)
	pending, err := receiver.queue.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile returned error: %v", err)
	}

	handler := New(receiver.session)
	result, err := handler.Handle("@reject 1")
	if err != nil {
		t.Fatalf("Handle(@reject 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Rejected file") {
		t.Fatalf("reject output = %q, want file rejection confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForNoOutgoingApprovalForCommands(t, sender.transfer, pending.RequestID)

	if entries, err := os.ReadDir(receiver.cfg.ReceivedFilesDir); err != nil {
		t.Fatalf("ReadDir(received files) returned error: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received files dir should stay empty after rejection, got %d entries", len(entries))
	}
}

func TestRejectSingleFolderOfferThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "reject-folder")
	writeFileStringForCommands(t, filepath.Join(folderPath, "README.md"), "reject folder readme")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder returned error: %v", err)
	}

	folderID := waitForPendingFolderCountForCommands(t, receiver.queue, 1)
	pending, err := receiver.queue.GetFolder(folderID)
	if err != nil {
		t.Fatalf("GetFolder returned error: %v", err)
	}

	handler := New(receiver.session)
	result, err := handler.Handle("@reject 1")
	if err != nil {
		t.Fatalf("Handle(@reject 1) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Rejected folder") {
		t.Fatalf("reject output = %q, want folder rejection confirmation", result.Output)
	}

	waitForNoPendingFoldersForCommands(t, receiver.queue)
	waitForNoOutgoingApprovalForCommands(t, sender.transfer, pending.RequestID)

	if entries, err := os.ReadDir(receiver.cfg.ReceivedFoldersDir); err != nil {
		t.Fatalf("ReadDir(received folders) returned error: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received folders dir should stay empty after rejection, got %d entries", len(entries))
	}
}

func TestApproveAllRequestsAndReceivesAllPendingFilesThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	firstPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "first.txt")
	secondPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "second.txt")
	writeFileStringForCommands(t, firstPath, "first approve-all payload")
	writeFileStringForCommands(t, secondPath, "second approve-all payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, firstPath); err != nil {
		t.Fatalf("SendFile(first) returned error: %v", err)
	}
	if err := sender.transfer.SendFile(receiverPeer, secondPath); err != nil {
		t.Fatalf("SendFile(second) returned error: %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 2)

	handler := New(receiver.session)
	result, err := handler.Handle("@approveAll")
	if err != nil {
		t.Fatalf("Handle(@approveAll) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Approved 2 queued items.") {
		t.Fatalf("approveAll output = %q, want bulk approval confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFilesDir, "first.txt"), "first approve-all payload")
	waitForFileContentForCommands(t, filepath.Join(receiver.cfg.ReceivedFilesDir, "second.txt"), "second approve-all payload")

	for requestID := range sender.transferPendingApprovals() {
		waitForNoOutgoingApprovalForCommands(t, sender.transfer, requestID)
	}
}

func TestRejectAllClearsPendingOffersThroughHandler(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	filePath := filepath.Join(sender.cfg.BaseDir, "fixtures", "reject-me.txt")
	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "folder-offer")
	writeFileStringForCommands(t, filePath, "reject-all file payload")
	writeFileStringForCommands(t, filepath.Join(folderPath, "README.md"), "folder manifest file")
	writeFileStringForCommands(t, filepath.Join(folderPath, "nested", "main.go"), "package main\n")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, filePath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}
	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder returned error: %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 1)
	waitForPendingFolderCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@rejectAll")
	if err != nil {
		t.Fatalf("Handle(@rejectAll) returned error: %v", err)
	}
	if !strings.Contains(result.Output, "Rejected 2 queued items.") {
		t.Fatalf("rejectAll output = %q, want bulk rejection confirmation", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForNoPendingFoldersForCommands(t, receiver.queue)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sender.transferPendingApprovals()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(sender.transferPendingApprovals()); got != 0 {
		t.Fatalf("sender still has %d outgoing approvals after rejectAll", got)
	}

	if entries, err := os.ReadDir(receiver.cfg.ReceivedFilesDir); err != nil {
		t.Fatalf("ReadDir(received files) returned error: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received files dir should be empty after rejectAll, got %d entries", len(entries))
	}
	if entries, err := os.ReadDir(receiver.cfg.ReceivedFoldersDir); err != nil {
		t.Fatalf("ReadDir(received folders) returned error: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("received folders dir should be empty after rejectAll, got %d entries", len(entries))
	}
}

func TestExitWarnsWhenPendingOffersExist(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	payloadPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "exit-warning.txt")
	writeFileStringForCommands(t, payloadPath, "pending exit warning payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, payloadPath); err != nil {
		t.Fatalf("SendFile returned error: %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)
	result, err := handler.Handle("@exit")
	if err != nil {
		t.Fatalf("Handle(@exit) returned error: %v", err)
	}
	if result.Quit {
		t.Fatal("@exit should not quit while pending offers exist")
	}
	if !strings.Contains(result.Output, "There are 1 pending approvals") {
		t.Fatalf("exit output = %q, want pending approval warning", result.Output)
	}
}

type commandTransferHarness struct {
	cfg       *config.Config
	log       *logger.Logger
	queue     *queue.Manager
	discovery *network.DiscoveryService
	transfer  *network.TransferService
	session   *session.Session
	events    chan events.Event
}

func newCommandTransferPair(t *testing.T) (*commandTransferHarness, *commandTransferHarness) {
	t.Helper()

	sender := newCommandTransferHarness(t, "sender", "127.0.0.1", 0, 47320, "shared-secret-for-command-tests")
	receiver := newCommandTransferHarness(t, "receiver", "127.0.0.1", 0, 47320, "shared-secret-for-command-tests")

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

	startCommandTransferHarness(t, sender)
	startCommandTransferHarness(t, receiver)
	linkCommandPeers(t, sender, receiver)
	linkCommandPeers(t, receiver, sender)
	return sender, receiver
}

func startCommandTransferHarness(t *testing.T, h *commandTransferHarness) {
	t.Helper()

	// Probing a free port releases it before Start can bind. Another test or
	// an outgoing connection can claim it in that gap, so retry a fresh port.
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		h.cfg.ListenPort = reserveTCPPortForCommands(t)
		if err = h.transfer.Start(h.cfg.Username, "127.0.0.1"); err == nil {
			return
		}
	}
	t.Fatalf("start %s transfer after 10 port attempts: %v", h.cfg.Username, err)
}

func newCommandTransferHarness(t *testing.T, username, ip string, listenPort, discoveryPort int, secret string) *commandTransferHarness {
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
	discovery := network.NewDiscoveryService(cfg, log)
	transfer := network.NewTransferService(cfg, log, hist, eventsCh, discovery, queueMgr)
	sess := session.New(cfg, log, hist, discovery, transfer, eventsCh, ip, queueMgr)

	return &commandTransferHarness{
		cfg:       cfg,
		log:       log,
		queue:     queueMgr,
		discovery: discovery,
		transfer:  transfer,
		session:   sess,
		events:    eventsCh,
	}
}

func restartReceiverHarness(t *testing.T, old *commandTransferHarness, sender *commandTransferHarness) *commandTransferHarness {
	t.Helper()

	log, err := logger.New(old.cfg.LogDir)
	if err != nil {
		t.Fatalf("logger.New restart returned error: %v", err)
	}

	queueMgr, err := queue.NewManager(old.cfg.BaseDir)
	if err != nil {
		t.Fatalf("queue.NewManager restart returned error: %v", err)
	}

	eventsCh := make(chan events.Event, 1024)
	hist := history.New(old.cfg)
	discovery := network.NewDiscoveryService(old.cfg, log)
	transfer := network.NewTransferService(old.cfg, log, hist, eventsCh, discovery, queueMgr)
	sess := session.New(old.cfg, log, hist, discovery, transfer, eventsCh, "127.0.0.1", queueMgr)

	restarted := &commandTransferHarness{
		cfg:       old.cfg,
		log:       log,
		queue:     queueMgr,
		discovery: discovery,
		transfer:  transfer,
		session:   sess,
		events:    eventsCh,
	}

	linkCommandPeers(t, restarted, sender)

	if err := restarted.transfer.Start(restarted.cfg.Username, "127.0.0.1"); err != nil {
		t.Fatalf("restart transfer start returned error: %v", err)
	}

	t.Cleanup(func() {
		restarted.transfer.Stop()
		_ = restarted.queue.Close()
		_ = restarted.log.Close()
	})

	return restarted
}

func restartSenderHarness(t *testing.T, old *commandTransferHarness, receiver *commandTransferHarness) *commandTransferHarness {
	t.Helper()

	log, err := logger.New(old.cfg.LogDir)
	if err != nil {
		t.Fatalf("logger.New sender restart returned error: %v", err)
	}

	queueMgr, err := queue.NewManager(old.cfg.BaseDir)
	if err != nil {
		t.Fatalf("queue.NewManager sender restart returned error: %v", err)
	}

	eventsCh := make(chan events.Event, 1024)
	hist := history.New(old.cfg)
	discovery := network.NewDiscoveryService(old.cfg, log)
	transfer := network.NewTransferService(old.cfg, log, hist, eventsCh, discovery, queueMgr)
	sess := session.New(old.cfg, log, hist, discovery, transfer, eventsCh, "127.0.0.1", queueMgr)

	restarted := &commandTransferHarness{
		cfg:       old.cfg,
		log:       log,
		queue:     queueMgr,
		discovery: discovery,
		transfer:  transfer,
		session:   sess,
		events:    eventsCh,
	}

	linkCommandPeers(t, restarted, receiver)

	if err := restarted.transfer.Start(restarted.cfg.Username, "127.0.0.1"); err != nil {
		t.Fatalf("restart sender transfer start returned error: %v", err)
	}

	t.Cleanup(func() {
		restarted.transfer.Stop()
		_ = restarted.queue.Close()
		_ = restarted.log.Close()
	})

	return restarted
}

func linkCommandPeers(t *testing.T, local, remote *commandTransferHarness) {
	t.Helper()

	pub, err := localPublicKeyForCommands(remote.cfg.Secret)
	if err != nil {
		t.Fatalf("localPublicKeyForCommands returned error: %v", err)
	}

	setDiscoveryPeerForCommands(t, local.discovery, remote.cfg.Username, "127.0.0.1", remote.cfg.ListenPort, pub)
}

func peerForCommandsHarness(t *testing.T, h *commandTransferHarness) *network.Peer {
	t.Helper()

	pub, err := localPublicKeyForCommands(h.cfg.Secret)
	if err != nil {
		t.Fatalf("localPublicKeyForCommands returned error: %v", err)
	}

	return &network.Peer{
		Username:  h.cfg.Username,
		IP:        "127.0.0.1",
		Port:      h.cfg.ListenPort,
		PublicKey: pub,
	}
}

func waitForPendingFileCountForCommands(t *testing.T, mgr *queue.Manager, want int) int {
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

func waitForPendingFolderCountForCommands(t *testing.T, mgr *queue.Manager, want int) int {
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

func waitForNoPendingFilesForCommands(t *testing.T, mgr *queue.Manager) {
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

func waitForNoPendingFoldersForCommands(t *testing.T, mgr *queue.Manager) {
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

func waitForNoOutgoingApprovalForCommands(t *testing.T, transfer *network.TransferService, requestID string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := getOutgoingApprovalForCommands(t, transfer, requestID); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for outgoing approval %q to be cleared", requestID)
}

func waitForFileContentForCommands(t *testing.T, path, want string) {
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

func (h *commandTransferHarness) transferPendingApprovals() map[string]struct{} {
	return listOutgoingApprovalsForCommands(h.transfer)
}

func reserveTCPPortForCommands(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserveTCPPortForCommands listen failed: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("reserveTCPPortForCommands unexpected addr type: %T", ln.Addr())
	}
	return addr.Port
}

func writeFileStringForCommands(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func localPublicKeyForCommands(secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("missing local secret")
	}
	seed := sha256.Sum256([]byte(secret))
	private := seed
	private[0] &= 248
	private[31] &= 127
	private[31] |= 64

	priv, err := ecdh.X25519().NewPrivateKey(private[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(priv.PublicKey().Bytes()), nil
}

func setDiscoveryPeerForCommands(t *testing.T, discovery *network.DiscoveryService, username, ip string, port int, publicKey string) {
	t.Helper()

	serviceValue := reflect.ValueOf(discovery).Elem()

	muField := serviceValue.FieldByName("mu")
	muPtr := reflect.NewAt(muField.Type(), unsafe.Pointer(muField.UnsafeAddr())).Elem().Addr().Interface().(*sync.RWMutex)
	muPtr.Lock()
	defer muPtr.Unlock()

	peersField := serviceValue.FieldByName("peers")
	peersValue := reflect.NewAt(peersField.Type(), unsafe.Pointer(peersField.UnsafeAddr())).Elem()

	peerType := peersValue.Type().Elem()
	peerValue := reflect.New(peerType.Elem())
	peerElem := peerValue.Elem()

	peerElem.FieldByName("Username").SetString(username)
	peerElem.FieldByName("IP").SetString(ip)
	peerElem.FieldByName("Port").SetInt(int64(port))
	peerElem.FieldByName("PublicKey").SetString(publicKey)
	peerElem.FieldByName("LastSeen").Set(reflect.ValueOf(time.Now()))

	peersValue.SetMapIndex(reflect.ValueOf(ip), peerValue)
}

func getOutgoingApprovalForCommands(t *testing.T, transfer *network.TransferService, requestID string) (reflect.Value, bool) {
	t.Helper()

	transferValue := reflect.ValueOf(transfer).Elem()

	muField := transferValue.FieldByName("outgoingMu")
	muPtr := reflect.NewAt(muField.Type(), unsafe.Pointer(muField.UnsafeAddr())).Elem().Addr().Interface().(*sync.Mutex)
	muPtr.Lock()
	defer muPtr.Unlock()

	approvalsField := transferValue.FieldByName("outgoingApprovals")
	approvalsValue := reflect.NewAt(approvalsField.Type(), unsafe.Pointer(approvalsField.UnsafeAddr())).Elem()

	value := approvalsValue.MapIndex(reflect.ValueOf(requestID))
	if !value.IsValid() {
		return reflect.Value{}, false
	}
	return value, true
}

func listOutgoingApprovalsForCommands(transfer *network.TransferService) map[string]struct{} {
	transferValue := reflect.ValueOf(transfer).Elem()

	muField := transferValue.FieldByName("outgoingMu")
	muPtr := reflect.NewAt(muField.Type(), unsafe.Pointer(muField.UnsafeAddr())).Elem().Addr().Interface().(*sync.Mutex)
	muPtr.Lock()
	defer muPtr.Unlock()

	approvalsField := transferValue.FieldByName("outgoingApprovals")
	approvalsValue := reflect.NewAt(approvalsField.Type(), unsafe.Pointer(approvalsField.UnsafeAddr())).Elem()

	out := make(map[string]struct{}, approvalsValue.Len())
	iter := approvalsValue.MapRange()
	for iter.Next() {
		out[iter.Key().String()] = struct{}{}
	}
	return out
}

// The reported failure: reject a folder, then @approveAll, and the files
// that were still pending never arrive. approveAll must resume from a
// queue that a previous rejection has already mutated.
func TestApproveAllAfterFolderRejectionStillDeliversPendingFiles(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	firstPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "keep-one.txt")
	secondPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "keep-two.txt")
	folderPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "drop-me")
	writeFileStringForCommands(t, firstPath, "first survives the rejection")
	writeFileStringForCommands(t, secondPath, "second survives the rejection")
	writeFileStringForCommands(t, filepath.Join(folderPath, "inside.txt"), "folder payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, firstPath); err != nil {
		t.Fatalf("SendFile(first): %v", err)
	}
	if err := sender.transfer.SendFolder(receiverPeer, folderPath); err != nil {
		t.Fatalf("SendFolder: %v", err)
	}
	if err := sender.transfer.SendFile(receiverPeer, secondPath); err != nil {
		t.Fatalf("SendFile(second): %v", err)
	}

	waitForPendingFileCountForCommands(t, receiver.queue, 2)
	folderID := waitForPendingFolderCountForCommands(t, receiver.queue, 1)

	handler := New(receiver.session)

	// Reject the folder from the middle of the queue.
	if _, err := handler.Handle(fmt.Sprintf("@reject %d", folderID)); err != nil {
		t.Fatalf("Handle(@reject folder): %v", err)
	}

	result, err := handler.Handle("@approveAll")
	if err != nil {
		t.Fatalf("Handle(@approveAll): %v", err)
	}
	if !strings.Contains(result.Output, "Approved 2 queued items.") {
		t.Fatalf("approveAll after rejection = %q, want both remaining files approved", result.Output)
	}

	waitForNoPendingFilesForCommands(t, receiver.queue)
	waitForFileContentForCommands(t,
		filepath.Join(receiver.cfg.ReceivedFilesDir, "keep-one.txt"),
		"first survives the rejection")
	waitForFileContentForCommands(t,
		filepath.Join(receiver.cfg.ReceivedFilesDir, "keep-two.txt"),
		"second survives the rejection")
}

// @queue has to describe the queue as it actually is after a partial
// approval, not as it was when the offers arrived.
func TestQueueOutputReflectsPartialResolution(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	keepPath := filepath.Join(sender.cfg.BaseDir, "fixtures", "still-waiting.txt")
	gonePath := filepath.Join(sender.cfg.BaseDir, "fixtures", "already-handled.txt")
	writeFileStringForCommands(t, keepPath, "still waiting payload")
	writeFileStringForCommands(t, gonePath, "already handled payload")

	receiverPeer := peerForCommandsHarness(t, receiver)
	if err := sender.transfer.SendFile(receiverPeer, gonePath); err != nil {
		t.Fatalf("SendFile(gone): %v", err)
	}
	if err := sender.transfer.SendFile(receiverPeer, keepPath); err != nil {
		t.Fatalf("SendFile(keep): %v", err)
	}
	waitForPendingFileCountForCommands(t, receiver.queue, 2)

	handler := New(receiver.session)
	before, err := handler.Handle("@queue")
	if err != nil {
		t.Fatalf("Handle(@queue) before: %v", err)
	}
	if !strings.Contains(before.Output, "still-waiting.txt") ||
		!strings.Contains(before.Output, "already-handled.txt") {
		t.Fatalf("@queue before resolution missing an item: %q", before.Output)
	}

	files := receiver.queue.ListFiles()
	var goneID int
	for _, f := range files {
		if f.Name == "already-handled.txt" {
			goneID = f.ID
		}
	}
	if goneID == 0 {
		t.Fatal("could not find the offer to reject")
	}
	if _, err := handler.Handle(fmt.Sprintf("@reject %d", goneID)); err != nil {
		t.Fatalf("Handle(@reject): %v", err)
	}

	after, err := handler.Handle("@queue")
	if err != nil {
		t.Fatalf("Handle(@queue) after: %v", err)
	}
	if strings.Contains(after.Output, "already-handled.txt") {
		t.Fatalf("@queue still lists a rejected offer: %q", after.Output)
	}
	if !strings.Contains(after.Output, "still-waiting.txt") {
		t.Fatalf("@queue dropped the offer that is still pending: %q", after.Output)
	}
}

// Offers arriving back to back must all reach the queue. A snapshot write
// failing, or one send overtaking another, previously lost them silently.
func TestRapidOffersAllReachTheQueue(t *testing.T) {
	sender, receiver := newCommandTransferPair(t)

	const count = 6
	receiverPeer := peerForCommandsHarness(t, receiver)
	for i := 0; i < count; i++ {
		path := filepath.Join(sender.cfg.BaseDir, "fixtures", fmt.Sprintf("burst-%d.txt", i))
		writeFileStringForCommands(t, path, fmt.Sprintf("burst payload %d", i))
		if err := sender.transfer.SendFile(receiverPeer, path); err != nil {
			t.Fatalf("SendFile(%d): %v", i, err)
		}
	}

	waitForPendingFileCountForCommands(t, receiver.queue, count)

	handler := New(receiver.session)
	result, err := handler.Handle("@queue")
	if err != nil {
		t.Fatalf("Handle(@queue): %v", err)
	}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("burst-%d.txt", i)
		if !strings.Contains(result.Output, name) {
			t.Fatalf("@queue is missing %s after a burst of offers: %q", name, result.Output)
		}
	}

	// IDs are handed out from one counter shared by files and folders, so
	// a burst must not produce a duplicate.
	seen := map[int]bool{}
	for _, f := range receiver.queue.ListFiles() {
		if seen[f.ID] {
			t.Fatalf("duplicate queue id %d", f.ID)
		}
		seen[f.ID] = true
	}
}
