package network

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/format"
	"github.com/bonjou-app/bonjou-cli/internal/history"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
)

const (
	kindMessage       = "message"
	kindFileOffer     = "file_offer"
	kindFolderOffer   = "folder_offer"
	kindFileRequest   = "file_request"
	kindFolderRequest = "folder_request"
	kindFileReject    = "file_reject"
	kindFolderReject  = "folder_reject"
	kindFile          = "file"
	kindFolder        = "folder"
	kindAck           = "ack"
	ackTimeout        = 12 * time.Second
)

var (
	errServiceStopping = errors.New("transfer service stopping")
	errApprovalMissing = errors.New("transfer approval request not found")
)

type envelope struct {
	Kind       string `json:"kind"`
	From       string `json:"from"`
	FromIP     string `json:"from_ip"`
	To         string `json:"to"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ActualSize int64  `json:"actual_size,omitempty"`
	Timestamp  int64  `json:"ts"`
	Message    string `json:"message"`
	Checksum   string `json:"checksum"`
	HMAC       string `json:"hmac"`
	AckKind    string `json:"ack_kind,omitempty"`
	AckStatus  string `json:"ack_status,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	TargetPath string `json:"target_path,omitempty"`
	// StreamID identifies the chunked-AEAD subkey used for the file/folder
	// payload that follows this envelope. Empty for non-streaming kinds.
	StreamID string `json:"stream_id,omitempty"`
}

// sealedEnvelope is the on-the-wire v2 envelope. Every field name is short
// because envelopes are frequent on the network. "v" gates compatibility:
// peers running an older protocol see a missing/wrong version and fail
// closed rather than try to decrypt with the legacy key schedule.
type sealedEnvelope struct {
	Version int    `json:"v"`
	Nonce   string `json:"n"`
	Payload string `json:"c"`
}

const sealedEnvelopeVersion = 2

type progressContext struct {
	id        string
	label     string
	path      string
	peer      string
	direction string
	kind      string
}

type outgoingApproval struct {
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ActualSize int64     `json:"actual_size"`
	Checksum   string    `json:"checksum"`
	CreatedAt  time.Time `json:"created_at"`
	// ArchivePath caches the folder's compressed zip path between offer
	// and approval. The receiver may take seconds to minutes to approve;
	// without this cache we'd zip the same directory twice (once to
	// compute the offer's checksum, once to send the bytes). Empty for
	// non-folder approvals.
	ArchivePath string `json:"archive_path,omitempty"`
}

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// TransferService manages TCP message and payload transfers.
type TransferService struct {
	cfg          *config.Config
	logger       *logger.Logger
	history      *history.Manager
	events       chan<- events.Event
	discovery    *DiscoveryService
	queue        *queue.Manager
	listener     net.Listener
	stop         chan struct{}
	stopOnce     sync.Once
	wait         sync.WaitGroup
	localUser    string
	localIP      string
	localMu      sync.RWMutex
	chunkSize    int
	chunkTimeout time.Duration
	replayCache  *replayCache

	pendingAckMu sync.Mutex
	pendingAcks  map[string]chan *envelope

	outgoingMu           sync.Mutex
	outgoingApprovals    map[string]*outgoingApproval
	outgoingSnapshotPath string

	cancelMu     sync.Mutex
	cancelActive chan struct{}
}

func (t *TransferService) isStopping() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

func NewTransferService(cfg *config.Config, logger *logger.Logger, history *history.Manager, events chan<- events.Event, discovery *DiscoveryService, queueMgr *queue.Manager) *TransferService {
	t := &TransferService{
		cfg:                  cfg,
		logger:               logger,
		history:              history,
		events:               events,
		discovery:            discovery,
		queue:                queueMgr,
		stop:                 make(chan struct{}),
		chunkSize:            cfg.ChunkSizeBytes(),
		chunkTimeout:         cfg.ChunkTimeout(),
		replayCache:          newReplayCache(),
		pendingAcks:          make(map[string]chan *envelope),
		outgoingApprovals:    make(map[string]*outgoingApproval),
		outgoingSnapshotPath: filepath.Join(cfg.BaseDir, "pending", "outgoing.json"),
	}
	t.loadOutgoingApprovals()
	return t
}

func (t *TransferService) Start(username, ip string) error {
	addr := fmt.Sprintf(":%d", t.cfg.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			return fmt.Errorf("port %d is already in use - another Bonjou instance or application may be running. Error: %v", t.cfg.ListenPort, err)
		}
		if strings.Contains(err.Error(), "permission denied") {
			return fmt.Errorf("permission denied to listen on port %d - you may need to use a port >1024 or run with elevated privileges. Error: %v", t.cfg.ListenPort, err)
		}
		return fmt.Errorf("failed to start transfer service on port %d: %v", t.cfg.ListenPort, err)
	}
	t.listener = ln
	t.localMu.Lock()
	t.localUser = username
	t.localIP = ip
	t.localMu.Unlock()
	t.wait.Add(1)
	go t.acceptLoop()
	return nil
}

func (t *TransferService) Stop() {
	t.stopOnce.Do(func() {
		close(t.stop)
		if t.listener != nil {
			_ = t.listener.Close()
		}
	})
	t.wait.Wait()
}

func (t *TransferService) acceptLoop() {
	defer t.wait.Done()
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.stop:
				return
			default:
			}
			t.logger.Error("accept error: %v", err)
			continue
		}
		t.wait.Add(1)
		go func(c net.Conn) {
			defer t.wait.Done()
			defer func() { _ = c.Close() }()
			if err := t.handleConnection(c); err != nil {
				if errors.Is(err, errServiceStopping) {
					return
				}
				t.logger.Error("handle connection: %v", err)
			}
		}(conn)
	}
}

func (t *TransferService) handleConnection(conn net.Conn) error {
	if t.isStopping() {
		return errServiceStopping
	}
	env, key, wireNonce, peerPub, err := t.readSecureEnvelope(conn)
	if err != nil {
		return err
	}
	if err := t.verifyEnvelope(env, key); err != nil {
		t.logger.Error("verify envelope failed: %v", err)
		t.emit(events.Event{Type: events.Error, Title: "Rejected incoming payload", Message: err.Error(), From: env.From, Timestamp: time.Now()})
		return err
	}
	// Reject replays and stale frames before doing any work. Both checks are
	// cheap and rely only on data we've already authenticated via the HMAC.
	if err := t.checkTimestampFreshness(env); err != nil {
		t.logger.Error("freshness rejected from %s: %v", env.From, err)
		return err
	}
	if err := t.replayCache.observe(peerPub, wireNonce); err != nil {
		t.logger.Error("replay rejected from %s: %v", env.From, err)
		return err
	}
	switch env.Kind {
	case kindMessage:
		// The envelope's GCM tag has already authenticated env.Message;
		// no separate per-field encryption is needed (v2 protocol).
		env.Message = normalizeMessageLineEndings(env.Message)
		t.emit(events.Event{Type: events.MessageReceived, Title: "Message", Message: env.Message, From: env.From, Timestamp: time.Now()})
		if t.history != nil {
			_ = t.history.AppendChat(env.From, env.To, env.Message)
		}
		return nil
	case kindFileOffer:
		return t.receiveFileOffer(env)
	case kindFolderOffer:
		return t.receiveFolderOffer(env)
	case kindFileRequest:
		return t.handleFileRequest(env)
	case kindFolderRequest:
		return t.handleFolderRequest(env)
	case kindFileReject, kindFolderReject:
		return t.handleTransferRejection(env)
	case kindFile:
		return t.receiveFile(conn, env, key)
	case kindFolder:
		return t.receiveFolder(conn, env, key)
	case kindAck:
		return t.receiveDeliveryAck(env)
	default:
		return fmt.Errorf("unknown payload kind: %s", env.Kind)
	}
}

// SendMessage delivers plain text to a peer.
func (t *TransferService) SendMessage(peer *Peer, message string) error {
	if t.isStopping() {
		return errServiceStopping
	}
	localUser, localIP := t.identity()
	message = normalizeMessageLineEndings(message)
	env := &envelope{
		Kind:      kindMessage,
		From:      localUser,
		FromIP:    localIP,
		To:        peer.Username,
		Message:   message,
		Timestamp: time.Now().Unix(),
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		return err
	}
	t.emit(events.Event{Type: events.MessageSent, Title: "Message sent", Message: message, To: peer.Username, Timestamp: time.Now()})
	if t.history != nil {
		return t.history.AppendChat(localUser, peer.Username, message)
	}
	return nil
}

// SendFile sends metadata first. File bytes are transferred only after receiver approval.
func (t *TransferService) SendFile(peer *Peer, path string) error {
	if t.isStopping() {
		return errServiceStopping
	}
	cancel, finish := t.beginCancelableOperation()
	defer finish()

	localUser, localIP := t.identity()
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("file not found: %s", filepath.Base(path))
		}
		return fmt.Errorf("cannot access file: %v", err)
	}
	if info.IsDir() {
		return errors.New("path is a directory — use @folder to send a folder")
	}
	if err := t.checkCanceled(cancel); err != nil {
		return err
	}
	tf, err := openTransferFile(path)
	if err != nil {
		return fmt.Errorf("cannot read file for transfer: %v", err)
	}
	_ = tf.file.Close()

	requestID, err := randomHexID(16)
	if err != nil {
		return err
	}
	t.storeOutgoingApproval(requestID, &outgoingApproval{
		Kind:      kindFile,
		Name:      filepath.Base(path),
		Path:      path,
		Size:      tf.size,
		Checksum:  tf.checksum,
		CreatedAt: time.Now(),
	})

	env := &envelope{
		Kind:      kindFileOffer,
		From:      localUser,
		FromIP:    localIP,
		To:        peer.Username,
		Name:      filepath.Base(path),
		Size:      tf.size,
		Checksum:  tf.checksum,
		RequestID: requestID,
		Timestamp: time.Now().Unix(),
		Message:   "waiting for receiver approval",
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		t.deleteOutgoingApproval(requestID)
		return err
	}

	t.emit(events.Event{
		Type:      events.Status,
		Title:     "File offer sent",
		Message:   fmt.Sprintf("File offer sent: '%s' to %s. Waiting for approval.", filepath.Base(path), peerLabelOrIP(peer)),
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	return nil
}

// SendFolder sends metadata first. Folder bytes are transferred only after receiver approval.
func (t *TransferService) SendFolder(peer *Peer, dir string) error {
	if t.isStopping() {
		return errServiceStopping
	}
	cancel, finish := t.beginCancelableOperation()
	defer finish()

	localUser, localIP := t.identity()
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("folder not found: %s", filepath.Base(dir))
		}
		return fmt.Errorf("cannot access folder: %v", err)
	}
	if !info.IsDir() {
		return errors.New("path is not a directory — use @file to send a single file")
	}

	displayName := filepath.Base(dir)
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Preparing folder offer",
		Message:   fmt.Sprintf("Preparing folder '%s' for approval...", displayName),
		To:        peer.Username,
		Timestamp: time.Now(),
	})

	actualSize, err := directorySize(dir, cancel)
	if err != nil {
		return fmt.Errorf("failed to measure folder size: %v", err)
	}

	preview, err := folderPreview(dir, cancel)
	if err != nil {
		return fmt.Errorf("failed to build folder preview: %v", err)
	}

	archivePath, err := zipDirectory(dir, cancel)
	if err != nil {
		return fmt.Errorf("failed to compress folder for transfer: %v", err)
	}
	// Note: archivePath is deliberately NOT deferred-removed here. The
	// approval record below carries it forward to
	// performApprovedFolderTransfer so the same bytes are reused when
	// the receiver approves. The transfer path is responsible for
	// cleaning up; if the offer fails before the approval lands, the
	// error path below removes it explicitly.

	tf, err := openTransferFile(archivePath)
	if err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("cannot read compressed archive for transfer: %v", err)
	}
	_ = tf.file.Close()

	requestID, err := randomHexID(16)
	if err != nil {
		_ = os.Remove(archivePath)
		return err
	}

	t.storeOutgoingApproval(requestID, &outgoingApproval{
		Kind:        kindFolder,
		Name:        displayName,
		Path:        dir,
		Size:        tf.size,
		ActualSize:  actualSize,
		Checksum:    tf.checksum,
		CreatedAt:   time.Now(),
		ArchivePath: archivePath,
	})

	env := &envelope{
		Kind:       kindFolderOffer,
		From:       localUser,
		FromIP:     localIP,
		To:         peer.Username,
		Name:       displayName,
		Size:       actualSize,
		ActualSize: actualSize,
		Checksum:   tf.checksum,
		RequestID:  requestID,
		Timestamp:  time.Now().Unix(),
		Message:    preview,
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		t.deleteOutgoingApproval(requestID)
		_ = os.Remove(archivePath)
		return err
	}

	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Folder offer sent",
		Message:   fmt.Sprintf("Folder offer sent: '%s' to %s. Waiting for approval.", displayName, peerLabelOrIP(peer)),
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	return nil
}

func (t *TransferService) ApproveFileTransfer(f *queue.PendingFile, destPath string) error {
	if f == nil {
		return queue.ErrQueueItemNotFound
	}
	peer, err := t.resolvePeerForQueueItem(f.Sender, f.SenderIP)
	if err != nil {
		return err
	}
	localUser, localIP := t.identity()
	env := &envelope{
		Kind:       kindFileRequest,
		From:       localUser,
		FromIP:     localIP,
		To:         peer.Username,
		Name:       f.Name,
		Size:       f.Size,
		RequestID:  f.RequestID,
		TargetPath: destPath,
		Timestamp:  time.Now().Unix(),
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		return err
	}
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Download requested",
		Message:   fmt.Sprintf("Requested file '%s' from %s", f.Name, peerLabelOrIP(peer)),
		From:      peer.Username,
		Timestamp: time.Now(),
	})
	return nil
}

func (t *TransferService) ApproveFolderTransfer(f *queue.PendingFolder, destPath string) error {
	if f == nil {
		return queue.ErrQueueItemNotFound
	}
	peer, err := t.resolvePeerForQueueItem(f.Sender, f.SenderIP)
	if err != nil {
		return err
	}
	localUser, localIP := t.identity()
	env := &envelope{
		Kind:       kindFolderRequest,
		From:       localUser,
		FromIP:     localIP,
		To:         peer.Username,
		Name:       f.Name,
		Size:       f.Size,
		RequestID:  f.RequestID,
		TargetPath: destPath,
		Timestamp:  time.Now().Unix(),
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		return err
	}
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Download requested",
		Message:   fmt.Sprintf("Requested folder '%s' from %s", f.Name, peerLabelOrIP(peer)),
		From:      peer.Username,
		Timestamp: time.Now(),
	})
	return nil
}

func (t *TransferService) RejectFileTransfer(f *queue.PendingFile) error {
	if f == nil {
		return queue.ErrQueueItemNotFound
	}
	return t.sendRejection(kindFileReject, f.RequestID, f.Name, f.Sender, f.SenderIP)
}

func (t *TransferService) RejectFolderTransfer(f *queue.PendingFolder) error {
	if f == nil {
		return queue.ErrQueueItemNotFound
	}
	return t.sendRejection(kindFolderReject, f.RequestID, f.Name, f.Sender, f.SenderIP)
}

func (t *TransferService) sendRejection(kind, requestID, name, sender, senderIP string) error {
	peer, err := t.resolvePeerForQueueItem(sender, senderIP)
	if err != nil {
		return err
	}
	localUser, localIP := t.identity()
	env := &envelope{
		Kind:      kind,
		From:      localUser,
		FromIP:    localIP,
		To:        peer.Username,
		Name:      name,
		RequestID: requestID,
		Timestamp: time.Now().Unix(),
	}
	if err := t.sendEnvelope(peer, env, nil); err != nil {
		return err
	}
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Transfer rejected",
		Message:   fmt.Sprintf("Rejected %s '%s' from %s", transferKindLabel(kind), name, peerLabelOrIP(peer)),
		From:      peer.Username,
		Timestamp: time.Now(),
	})
	return nil
}

// emitOfferRejected tells the user an offer arrived and could not be
// queued. Silence here reads as a dropped transfer with no explanation,
// which is indistinguishable from the network failing.
func (t *TransferService) emitOfferRejected(kind, from, name string, cause error) {
	t.emit(events.Event{
		Type:      events.Error,
		Title:     "Incoming " + kind + " refused",
		Message:   fmt.Sprintf("Could not queue %s from %s: %v", name, from, cause),
		From:      from,
		Timestamp: time.Now(),
		Level:     "error",
	})
}

func (t *TransferService) receiveFileOffer(env *envelope) error {
	if err := t.validateIncomingSize(env.Size, "file"); err != nil {
		return err
	}
	queueID, err := t.queue.AddFile(env.RequestID, env.From, env.FromIP, env.Name, env.Size, "")
	if err != nil {
		// Returning here used to be the whole story, which meant a
		// refused offer produced no queue entry and no notification: the
		// sender saw it leave and the receiver saw nothing at all.
		t.emitOfferRejected("file", env.From, env.Name, err)
		return err
	}
	displayMsg := fmt.Sprintf(
		"Pending file [%d] from %s: %s (%s)\nRun @queue, @view %d, @approve %d, or @reject %d",
		queueID,
		env.From,
		env.Name,
		formatSize(env.Size),
		queueID,
		queueID,
		queueID,
	)
	t.emit(events.Event{
		Type:      events.FilePending,
		Title:     "File pending approval",
		Message:   displayMsg,
		From:      env.From,
		Size:      env.Size,
		Timestamp: time.Now(),
		Level:     "info",
	})
	return nil
}

func (t *TransferService) receiveFolderOffer(env *envelope) error {
	if err := t.validateIncomingSize(env.Size, "folder"); err != nil {
		return err
	}
	queueID, err := t.queue.AddFolder(env.RequestID, env.From, env.FromIP, env.Name, env.Size, env.Message)
	if err != nil {
		t.emitOfferRejected("folder", env.From, env.Name, err)
		return err
	}
	displayMsg := fmt.Sprintf(
		"Pending folder [%d] from %s: %s (%s)\nRun @queue, @view %d, @approve %d, or @reject %d",
		queueID,
		env.From,
		env.Name,
		formatSize(env.Size),
		queueID,
		queueID,
		queueID,
	)
	t.emit(events.Event{
		Type:      events.FolderPending,
		Title:     "Folder pending approval",
		Message:   displayMsg,
		From:      env.From,
		Size:      env.Size,
		Timestamp: time.Now(),
		Level:     "info",
	})
	return nil
}

func (t *TransferService) handleFileRequest(env *envelope) error {
	approval, ok := t.getOutgoingApproval(env.RequestID)
	if !ok {
		return errApprovalMissing
	}
	if !strings.EqualFold(approval.Kind, kindFile) {
		return fmt.Errorf("approval kind mismatch for request %s", env.RequestID)
	}
	return t.performApprovedFileTransfer(env, approval)
}

func (t *TransferService) handleFolderRequest(env *envelope) error {
	approval, ok := t.getOutgoingApproval(env.RequestID)
	if !ok {
		return errApprovalMissing
	}
	if !strings.EqualFold(approval.Kind, kindFolder) {
		return fmt.Errorf("approval kind mismatch for request %s", env.RequestID)
	}
	return t.performApprovedFolderTransfer(env, approval)
}

func (t *TransferService) handleTransferRejection(env *envelope) error {
	approval, ok := t.getOutgoingApproval(env.RequestID)
	if ok {
		// Remove the temp zip we created for the offer; otherwise rejected
		// folder offers leak a copy of the user's content under /tmp until
		// the OS cleans it up.
		if approval.ArchivePath != "" {
			_ = os.Remove(approval.ArchivePath)
		}
		t.deleteOutgoingApproval(env.RequestID)
		t.emit(events.Event{
			Type:      events.Status,
			Title:     "Transfer rejected",
			Message:   fmt.Sprintf("%s '%s' was rejected by %s", transferKindLabel(approval.Kind), approval.Name, safeRemoteLabel(env.From, env.FromIP)),
			From:      env.From,
			Timestamp: time.Now(),
		})
		return nil
	}
	return nil
}

func (t *TransferService) performApprovedFileTransfer(env *envelope, approval *outgoingApproval) error {
	cancel, finish := t.beginCancelableOperation()
	defer finish()

	peer, err := t.resolvePeerForQueueItem(env.From, env.FromIP)
	if err != nil {
		return err
	}
	tf, err := openTransferFile(approval.Path)
	if err != nil {
		return fmt.Errorf("cannot read file for transfer: %v", err)
	}
	defer func() { _ = tf.file.Close() }()

	localUser, localIP := t.identity()
	sendEnv := &envelope{
		Kind:      kindFile,
		From:      localUser,
		FromIP:    localIP,
		To:        peer.Username,
		Name:      approval.Name,
		Size:      tf.size,
		Checksum:  tf.checksum,
		RequestID: env.RequestID,
		Message:   env.TargetPath,
		Timestamp: time.Now().Unix(),
	}
	stream := func(writer io.Writer) error {
		ctx := progressContext{
			id:        fmt.Sprintf("file:%s", sendEnv.Name),
			label:     fmt.Sprintf("Sending %s", sendEnv.Name),
			path:      approval.Path,
			peer:      formatPeer(peer),
			direction: "send",
			kind:      kindFile,
		}
		return t.copyWithProgress(writer, tf.file, tf.size, ctx, cancel)
	}
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Approval received",
		Message:   fmt.Sprintf("%s approved file '%s'. Starting upload...", peerLabelOrIP(peer), approval.Name),
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	t.emit(events.Event{
		Type:      events.FileSent,
		Title:     "File upload started",
		Message:   approval.Name,
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	if err := t.sendEnvelope(peer, sendEnv, stream); err != nil {
		t.emitTransferIssue(kindFile, approval.Name, peer.Username, approval.Path, err)
		return err
	}
	t.deleteOutgoingApproval(env.RequestID)
	if t.history != nil {
		return t.history.AppendTransfer(localUser, peer.Username, approval.Path, tf.size, kindFile)
	}
	return nil
}

func (t *TransferService) performApprovedFolderTransfer(env *envelope, approval *outgoingApproval) error {
	cancel, finish := t.beginCancelableOperation()
	defer finish()

	peer, err := t.resolvePeerForQueueItem(env.From, env.FromIP)
	if err != nil {
		return err
	}

	// Reuse the zip we built during SendFolder if it still exists on disk
	// (the common case — folder approvals tend to land within seconds).
	// If the archive was removed (e.g. /tmp cleanup, process restart) we
	// fall back to zipping again so the transfer still works.
	archivePath := approval.ArchivePath
	if archivePath != "" {
		if _, statErr := os.Stat(archivePath); statErr != nil {
			archivePath = ""
		}
	}
	if archivePath == "" {
		archivePath, err = zipDirectory(approval.Path, cancel)
		if err != nil {
			return fmt.Errorf("failed to compress folder for transfer: %v", err)
		}
	}
	defer func() { _ = os.Remove(archivePath) }()

	tf, err := openTransferFile(archivePath)
	if err != nil {
		return fmt.Errorf("cannot read compressed archive for transfer: %v", err)
	}
	defer func() { _ = tf.file.Close() }()

	localUser, localIP := t.identity()
	sendEnv := &envelope{
		Kind:       kindFolder,
		From:       localUser,
		FromIP:     localIP,
		To:         peer.Username,
		Name:       approval.Name + ".zip",
		Size:       tf.size,
		ActualSize: approval.ActualSize,
		Checksum:   tf.checksum,
		RequestID:  env.RequestID,
		Message:    env.TargetPath,
		Timestamp:  time.Now().Unix(),
	}
	stream := func(writer io.Writer) error {
		ctx := progressContext{
			id:        fmt.Sprintf("folder:%s", approval.Name),
			label:     fmt.Sprintf("Sending %s", approval.Name),
			path:      approval.Path,
			peer:      formatPeer(peer),
			direction: "send",
			kind:      kindFolder,
		}
		return t.copyWithProgress(writer, tf.file, tf.size, ctx, cancel)
	}
	t.emit(events.Event{
		Type:      events.Status,
		Title:     "Approval received",
		Message:   fmt.Sprintf("%s approved folder '%s'. Starting upload...", peerLabelOrIP(peer), approval.Name),
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	t.emit(events.Event{
		Type:      events.FolderSent,
		Title:     "Folder upload started",
		Message:   approval.Name,
		To:        peer.Username,
		Timestamp: time.Now(),
	})
	if err := t.sendEnvelope(peer, sendEnv, stream); err != nil {
		t.emitTransferIssue(kindFolder, approval.Name, peer.Username, approval.Path, err)
		return err
	}
	t.deleteOutgoingApproval(env.RequestID)
	if t.history != nil {
		return t.history.AppendTransfer(localUser, peer.Username, approval.Path, tf.size, kindFolder)
	}
	return nil
}

// sendEnvelope dials the peer, transmits one v2 sealed envelope, and (when
// a streamingWriter is supplied for file/folder kinds) streams a chunked
// AEAD payload that follows it on the same TCP connection.
//
// The writer callback receives an io.Writer that already encrypts and
// frames every Write call under a per-stream AEAD subkey. Callers just
// io.Copy plaintext into it; no cipher.Stream gymnastics required.
func (t *TransferService) sendEnvelope(peer *Peer, env *envelope, writer func(io.Writer) error) error {
	if t.isStopping() {
		return errServiceStopping
	}
	if strings.TrimSpace(peer.PublicKey) == "" {
		return errors.New("peer public key unknown; update peer and rediscover")
	}
	shared, err := t.sharedKey(peer.PublicKey)
	if err != nil {
		return err
	}

	// File/folder payloads need a per-stream subkey, derived from a random
	// streamID announced in the envelope. Each stream therefore uses fresh
	// AEAD key material, making chunk-nonce reuse across streams
	// structurally impossible.
	var streamKey []byte
	if writer != nil && (env.Kind == kindFile || env.Kind == kindFolder) {
		sid, err := randomStreamID()
		if err != nil {
			return err
		}
		env.StreamID = hex.EncodeToString(sid)
		streamKey, err = deriveStreamKey(shared, sid)
		if err != nil {
			return err
		}
	}
	env.HMAC = t.signEnvelope(env, shared)

	address := net.JoinHostPort(peer.IP, fmt.Sprintf("%d", peer.Port))
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	sealed, err := sealEnvelope(env, shared)
	if err != nil {
		return err
	}
	if err := writeEnvelope(conn, sealed); err != nil {
		return err
	}

	if writer == nil {
		return nil
	}
	if t.isStopping() {
		return errServiceStopping
	}

	var ackKey string
	var ackCh chan *envelope
	if env.Kind == kindFile || env.Kind == kindFolder {
		ackKey = deliveryAckKey(env.Kind, transferDisplayName(env.Kind, env.Name), peerLabelOrIP(peer))
		ackCh = t.registerPendingAck(ackKey)
		defer t.unregisterPendingAck(ackKey)
	}

	encStream, err := newChunkedFrameWriter(conn, streamKey)
	if err != nil {
		return err
	}
	if err := writer(encStream); err != nil {
		return err
	}
	if env.Kind == kindFile || env.Kind == kindFolder {
		if err := t.awaitInlineDeliveryAck(conn, shared, env); err != nil {
			if ackCh != nil {
				if ack := t.waitPendingAck(ackCh, 2*time.Second); ack != nil {
					if strings.EqualFold(ack.AckStatus, "ok") {
						return nil
					}
					return fmt.Errorf("delivery failed: %s '%s' to %s", transferKindLabel(env.Kind), transferDisplayName(env.Kind, env.Name), peerLabelOrIP(peer))
				}
			}
			return err
		}
	}
	return nil
}

// signEnvelope computes an HMAC over the envelope fields with a key derived
// separately from the AES-GCM envelope key (HKDF, info="bonjou/v2/mac").
// Every field is length-prefixed so adjacent strings cannot be ambiguously
// concatenated.
//
// With v2 sealed envelopes (AES-256-GCM) this MAC is redundant for
// authenticity — GCM already protects every byte. It is kept to preserve
// the existing field-level signature semantics that downstream code asserts
// against (e.g. delivery-ack verification).
func (t *TransferService) signEnvelope(env *envelope, key []byte) string {
	macKey, err := deriveMACKey(key)
	if err != nil {
		return ""
	}
	sizeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBuf, uint64(env.Size))
	actualSizeBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(actualSizeBuf, uint64(env.ActualSize))
	tsBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBuf, uint64(env.Timestamp))
	mac := hmac.New(sha256.New, macKey)
	mac.Write(encodeMACFields(
		[]byte(env.Kind),
		[]byte(env.From),
		[]byte(env.FromIP),
		[]byte(env.To),
		[]byte(env.Name),
		[]byte(env.Message),
		[]byte(env.Checksum),
		[]byte(env.RequestID),
		[]byte(env.TargetPath),
		[]byte(env.AckKind),
		[]byte(env.AckStatus),
		[]byte(env.StreamID),
		sizeBuf,
		actualSizeBuf,
		tsBuf,
	))
	return hex.EncodeToString(mac.Sum(nil))
}

func (t *TransferService) verifyEnvelope(env *envelope, key []byte) error {
	if len(key) == 0 {
		return errors.New("missing envelope key")
	}
	expected := t.signEnvelope(env, key)
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return fmt.Errorf("unable to compute signature for %s: %w", env.From, err)
	}
	providedBytes, err := hex.DecodeString(env.HMAC)
	if err != nil {
		return fmt.Errorf("invalid signature data from %s: %w", env.From, err)
	}
	if !hmac.Equal(expectedBytes, providedBytes) {
		return fmt.Errorf("discarded %s from %s (%s): signature mismatch", env.Kind, env.From, env.FromIP)
	}
	return nil
}

func (t *TransferService) receiveFile(conn net.Conn, env *envelope, key []byte) error {
	if t.isStopping() {
		return errServiceStopping
	}
	if err := t.validateIncomingSize(env.Size, "file"); err != nil {
		return err
	}
	fileName := filepath.Base(strings.TrimSpace(env.Name))
	if fileName == "" || fileName == "." || fileName == string(os.PathSeparator) {
		return errors.New("invalid file name in transfer")
	}
	destPath := strings.TrimSpace(env.Message)
	if destPath == "" {
		destPath = filepath.Join(t.cfg.ReceivedFilesDir, fileName)
	}
	destPath = queue.UniquePath(destPath)
	if err := ensurePathWithinRoot(destPath, t.cfg.ReceivedFilesDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(destPath)
		}
	}()

	hasher := sha256.New()
	defer func() {
		_ = file.Close()
	}()
	ctx := progressContext{
		id:        fmt.Sprintf("recv:%s", fileName),
		label:     fmt.Sprintf("Receiving %s", fileName),
		path:      destPath,
		peer:      formatRemote(env.From, env.FromIP),
		direction: "receive",
		kind:      kindFile,
	}

	frameReader, err := t.openStreamReader(conn, key, env)
	if err != nil {
		return err
	}
	if err := t.readWithProgress(frameReader, file, env.Size, hasher, ctx); err != nil {
		t.emitTransferIssue(kindFile, fileName, env.From, destPath, err)
		return err
	}

	receivedChecksum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(receivedChecksum, env.Checksum) {
		if err := t.writeInlineDeliveryAck(conn, key, env, kindFile, fileName, "error"); err != nil {
			t.sendDeliveryAckOutOfBand(env, kindFile, fileName, "error")
		}
		msg := fmt.Sprintf("File '%s' from %s did not arrive intact — data was corrupted in transit. Please ask them to send it again.", fileName, env.From)
		t.emit(events.Event{Type: events.Error, Title: "Transfer integrity check failed", Message: msg, From: env.From, Timestamp: time.Now(), Path: destPath})
		return fmt.Errorf("transfer integrity check failed for '%s'", fileName)
	}

	if err := file.Close(); err != nil {
		return err
	}
	cleanup = false

	if err := t.writeInlineDeliveryAck(conn, key, env, kindFile, fileName, "ok"); err != nil {
		t.sendDeliveryAckOutOfBand(env, kindFile, fileName, "ok")
	}

	t.emit(events.Event{
		Type:      events.FileReceived,
		Title:     "File received",
		Message:   fileName,
		From:      env.From,
		Path:      destPath,
		Size:      env.Size,
		Timestamp: time.Now(),
	})
	if t.history != nil {
		_ = t.history.AppendTransfer(env.From, t.localUsername(), destPath, env.Size, kindFile)
	}
	return nil
}

func (t *TransferService) receiveFolder(conn net.Conn, env *envelope, key []byte) error {
	if t.isStopping() {
		return errServiceStopping
	}
	if err := t.validateIncomingSize(env.Size, "folder"); err != nil {
		return err
	}
	pendingDir := filepath.Join(os.TempDir(), "bonjou-pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		return err
	}
	archiveName := filepath.Base(strings.TrimSpace(env.Name))
	if archiveName == "" || archiveName == "." || archiveName == string(os.PathSeparator) {
		return errors.New("invalid folder archive name in transfer")
	}
	tempPath := queue.UniquePath(filepath.Join(pendingDir, "raw-"+archiveName))
	if err := ensurePathWithinRoot(tempPath, pendingDir); err != nil {
		return err
	}
	file, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}()

	destDirName := strings.TrimSuffix(archiveName, ".zip")
	destDir := strings.TrimSpace(env.Message)
	if destDir == "" {
		destDir = filepath.Join(t.cfg.ReceivedFoldersDir, destDirName)
	}
	destDir = queue.UniquePath(destDir)
	if err := ensurePathWithinRoot(destDir, t.cfg.ReceivedFoldersDir); err != nil {
		return err
	}

	hasher := sha256.New()
	ctx := progressContext{
		id:        fmt.Sprintf("recv:%s", archiveName),
		label:     fmt.Sprintf("Receiving %s", destDirName),
		path:      destDir,
		peer:      formatRemote(env.From, env.FromIP),
		direction: "receive",
		kind:      kindFolder,
	}

	frameReader, err := t.openStreamReader(conn, key, env)
	if err != nil {
		return err
	}
	if err := t.readWithProgress(frameReader, file, env.Size, hasher, ctx); err != nil {
		t.emitTransferIssue(kindFolder, destDirName, env.From, destDir, err)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	receivedChecksum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(receivedChecksum, env.Checksum) {
		if err := t.writeInlineDeliveryAck(conn, key, env, kindFolder, destDirName, "error"); err != nil {
			t.sendDeliveryAckOutOfBand(env, kindFolder, destDirName, "error")
		}
		msg := fmt.Sprintf("Folder '%s' from %s did not arrive intact — data was corrupted in transit. Please ask them to send it again.", destDirName, env.From)
		t.emit(events.Event{Type: events.Error, Title: "Transfer integrity check failed", Message: msg, From: env.From, Timestamp: time.Now(), Path: tempPath})
		return fmt.Errorf("transfer integrity check failed for folder '%s'", destDirName)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	if err := unzip(tempPath, destDir); err != nil {
		_ = os.RemoveAll(destDir)
		if ackErr := t.writeInlineDeliveryAck(conn, key, env, kindFolder, destDirName, "error"); ackErr != nil {
			t.sendDeliveryAckOutOfBand(env, kindFolder, destDirName, "error")
		}
		return err
	}

	if err := t.writeInlineDeliveryAck(conn, key, env, kindFolder, destDirName, "ok"); err != nil {
		t.sendDeliveryAckOutOfBand(env, kindFolder, destDirName, "ok")
	}

	displaySize := env.Size
	if env.ActualSize > 0 {
		displaySize = env.ActualSize
	}
	t.emit(events.Event{
		Type:      events.FolderReceived,
		Title:     "Folder received",
		Message:   destDirName,
		From:      env.From,
		Path:      destDir,
		Size:      displaySize,
		Timestamp: time.Now(),
	})
	if t.history != nil {
		_ = t.history.AppendTransfer(env.From, t.localUsername(), destDir, displaySize, kindFolder)
	}
	return nil
}

// openStreamReader builds the chunked-AEAD reader for an inbound file or
// folder stream. The per-stream subkey is derived from the shared secret +
// envelope.StreamID, the same derivation the sender used.
func (t *TransferService) openStreamReader(src io.Reader, shared []byte, env *envelope) (io.Reader, error) {
	if strings.TrimSpace(env.StreamID) == "" {
		return nil, errors.New("missing stream_id on encrypted payload")
	}
	streamID, err := hex.DecodeString(env.StreamID)
	if err != nil {
		return nil, fmt.Errorf("decode stream id: %w", err)
	}
	streamKey, err := deriveStreamKey(shared, streamID)
	if err != nil {
		return nil, err
	}
	return newChunkedFrameReader(src, streamKey)
}

func (t *TransferService) copyWithProgress(writer io.Writer, reader io.Reader, total int64, ctx progressContext, cancel <-chan struct{}) error {
	chunkSize := t.chunkSize
	if chunkSize <= 0 {
		chunkSize = 64 * 1024
	}
	buf := make([]byte, chunkSize)
	var sent int64
	started := time.Now()
	deadline := t.chunkTimeout
	var setter writeDeadlineSetter
	if deadline > 0 {
		if s, ok := writer.(writeDeadlineSetter); ok {
			setter = s
			defer func() { _ = setter.SetWriteDeadline(time.Time{}) }()
		}
	}
	for {
		if t.isStopping() {
			return errServiceStopping
		}
		if err := t.checkCanceled(cancel); err != nil {
			return err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			payload := buf[:n]
			if setter != nil && deadline > 0 {
				_ = setter.SetWriteDeadline(time.Now().Add(deadline))
			}
			// writer here is the chunkedFrameWriter, which seals each chunk
			// under AES-GCM before pushing to the network. The plaintext
			// never leaves this goroutine in unencrypted form.
			if _, err := writer.Write(payload); err != nil {
				return err
			}
			sent += int64(n)
			t.emit(events.Event{Type: events.Progress, Progress: &events.ProgressState{
				ID:        ctx.id,
				Label:     ctx.label,
				Path:      ctx.path,
				Peer:      ctx.peer,
				Direction: ctx.direction,
				Kind:      ctx.kind,
				Current:   sent,
				Total:     total,
				StartedAt: started,
				UpdatedAt: time.Now(),
			}})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	t.emit(events.Event{Type: events.Progress, Progress: &events.ProgressState{
		ID:        ctx.id,
		Label:     ctx.label,
		Path:      ctx.path,
		Peer:      ctx.peer,
		Direction: ctx.direction,
		Kind:      ctx.kind,
		Current:   total,
		Total:     total,
		StartedAt: started,
		UpdatedAt: time.Now(),
		Done:      true,
	}})
	return nil
}

func (t *TransferService) readWithProgress(reader io.Reader, writer io.Writer, total int64, hash io.Writer, ctx progressContext) error {
	chunkSize := t.chunkSize
	if chunkSize <= 0 {
		chunkSize = 64 * 1024
	}
	buf := make([]byte, chunkSize)
	var received int64
	multiWriter := io.MultiWriter(writer, hash)
	started := time.Now()
	deadline := t.chunkTimeout
	var setter readDeadlineSetter
	if deadline > 0 {
		if s, ok := reader.(readDeadlineSetter); ok {
			setter = s
			defer func() { _ = setter.SetReadDeadline(time.Time{}) }()
		}
	}
	for received < total {
		if t.isStopping() {
			return errServiceStopping
		}
		remaining := total - received
		chunk := buf
		if int64(len(chunk)) > remaining {
			chunk = buf[:remaining]
		}
		if setter != nil && deadline > 0 {
			_ = setter.SetReadDeadline(time.Now().Add(deadline))
		}
		// reader is the chunkedFrameReader: each Read pulls and decrypts
		// one AEAD chunk and surfaces its plaintext. Tampering is detected
		// inside Read, before any byte reaches multiWriter.
		n, err := reader.Read(chunk)
		if err != nil && n == 0 {
			return err
		}
		if n == 0 {
			continue
		}
		payload := chunk[:n]
		if _, err := multiWriter.Write(payload); err != nil {
			return err
		}
		received += int64(n)
		if !strings.EqualFold(ctx.direction, "receive") {
			t.emit(events.Event{Type: events.Progress, Progress: &events.ProgressState{
				ID:        ctx.id,
				Label:     ctx.label,
				Path:      ctx.path,
				Peer:      ctx.peer,
				Direction: ctx.direction,
				Kind:      ctx.kind,
				Current:   received,
				Total:     total,
				StartedAt: started,
				UpdatedAt: time.Now(),
			}})
		}
	}
	if !strings.EqualFold(ctx.direction, "receive") {
		t.emit(events.Event{Type: events.Progress, Progress: &events.ProgressState{
			ID:        ctx.id,
			Label:     ctx.label,
			Path:      ctx.path,
			Peer:      ctx.peer,
			Direction: ctx.direction,
			Kind:      ctx.kind,
			Current:   total,
			Total:     total,
			StartedAt: started,
			UpdatedAt: time.Now(),
			Done:      true,
		}})
	}
	return nil
}

func (t *TransferService) emit(evt events.Event) {
	if evt.Type == events.Progress {
		select {
		case t.events <- evt:
		default:
		}
		return
	}
	select {
	case t.events <- evt:
	default:
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case t.events <- evt:
		case <-timer.C:
			t.logger.Error("dropping event %s because the event channel is saturated", evt.Type)
		}
	}
}

func (t *TransferService) receiveDeliveryAck(env *envelope) error {
	t.notifyPendingAck(env)
	t.renderDeliveryAck(env)
	return nil
}

func (t *TransferService) awaitInlineDeliveryAck(conn net.Conn, shared []byte, sent *envelope) error {
	if conn == nil {
		return errors.New("missing transfer connection")
	}
	if setter, ok := conn.(readDeadlineSetter); ok {
		_ = setter.SetReadDeadline(time.Now().Add(ackTimeout))
		defer func() { _ = setter.SetReadDeadline(time.Time{}) }()
	}
	frame, err := readEnvelope(conn)
	if err != nil {
		return fmt.Errorf("no delivery confirmation from '%s' for %s '%s' — the transfer may or may not have completed", sent.To, transferKindLabel(sent.Kind), transferDisplayName(sent.Kind, sent.Name))
	}
	ack, _, err := openEnvelope(frame, shared)
	if err != nil {
		return fmt.Errorf("invalid delivery confirmation: %w", err)
	}
	if ack.Kind != kindAck {
		return fmt.Errorf("unexpected response kind '%s' during transfer confirmation", ack.Kind)
	}
	if err := t.verifyEnvelopeWithKey(ack, shared); err != nil {
		return fmt.Errorf("invalid delivery confirmation signature: %w", err)
	}
	if ack.AckKind != "" && !strings.EqualFold(ack.AckKind, sent.Kind) {
		return fmt.Errorf("delivery confirmation kind mismatch: sent=%s confirmed=%s", sent.Kind, ack.AckKind)
	}
	t.renderDeliveryAck(ack)
	if !strings.EqualFold(ack.AckStatus, "ok") {
		return fmt.Errorf("delivery failed: %s '%s' to %s", transferKindLabel(sent.Kind), transferDisplayName(sent.Kind, sent.Name), strings.TrimSpace(sent.To))
	}
	return nil
}

func (t *TransferService) writeInlineDeliveryAck(conn net.Conn, key []byte, source *envelope, kind, name, status string) error {
	if conn == nil {
		return errors.New("missing transfer connection")
	}
	localUser, localIP := t.identity()
	ack := &envelope{
		Kind:      kindAck,
		From:      localUser,
		FromIP:    localIP,
		To:        source.From,
		Name:      name,
		Timestamp: time.Now().Unix(),
		AckKind:   kind,
		AckStatus: status,
	}
	ack.HMAC = t.signEnvelope(ack, key)
	sealed, err := sealEnvelope(ack, key)
	if err != nil {
		return err
	}
	return writeEnvelope(conn, sealed)
}

func (t *TransferService) verifyEnvelopeWithKey(env *envelope, key []byte) error {
	expected := t.signEnvelope(env, key)
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return fmt.Errorf("unable to compute signature: %w", err)
	}
	providedBytes, err := hex.DecodeString(env.HMAC)
	if err != nil {
		return fmt.Errorf("invalid signature data: %w", err)
	}
	if !hmac.Equal(expectedBytes, providedBytes) {
		return errors.New("signature mismatch")
	}
	return nil
}

func (t *TransferService) renderDeliveryAck(env *envelope) {
	kindLabel := transferKindLabel(env.AckKind)
	name := strings.TrimSpace(env.Name)
	if name == "" {
		name = "payload"
	}
	peer := strings.TrimSpace(env.From)
	if peer == "" {
		peer = "peer"
	}
	if strings.EqualFold(env.AckStatus, "ok") {
		message := fmt.Sprintf("Delivered: %s '%s' to %s", kindLabel, name, peer)
		title := "Delivery confirmed"
		t.emit(events.Event{
			Type:      events.Status,
			Title:     title,
			Message:   message,
			From:      env.From,
			Timestamp: time.Now(),
		})
		return
	}
	t.emit(events.Event{
		Type:      events.Error,
		Title:     "Delivery failed",
		Message:   fmt.Sprintf("Delivery failed: %s '%s' to %s", kindLabel, name, peer),
		From:      env.From,
		Timestamp: time.Now(),
	})
}

func (t *TransferService) sendDeliveryAckOutOfBand(source *envelope, kind, name, status string) {
	peer, err := t.resolvePeerForAck(source)
	if err != nil {
		t.logger.Error("delivery ack fallback resolve peer: %v", err)
		return
	}
	localUser, localIP := t.identity()
	ack := &envelope{
		Kind:      kindAck,
		From:      localUser,
		FromIP:    localIP,
		To:        source.From,
		Name:      transferDisplayName(kind, name),
		Timestamp: time.Now().Unix(),
		AckKind:   kind,
		AckStatus: status,
	}
	if err := t.sendEnvelope(peer, ack, nil); err != nil {
		t.logger.Error("delivery ack fallback send failed: %v", err)
	}
}

func (t *TransferService) resolvePeerForAck(source *envelope) (*Peer, error) {
	ip := strings.TrimSpace(source.FromIP)
	if ip == "" {
		return nil, errors.New("missing sender ip for ack")
	}
	if t.discovery != nil {
		if peer, err := t.discovery.Resolve(ip); err == nil {
			return peer, nil
		}
	}
	if t.discovery == nil {
		return nil, errors.New("discovery service unavailable")
	}
	publicKey, ok := t.discovery.SharedPublicKey(source.From, ip)
	if !ok || strings.TrimSpace(publicKey) == "" {
		return nil, fmt.Errorf("peer key unavailable for %s (%s)", source.From, ip)
	}
	return &Peer{Username: source.From, IP: ip, Port: t.cfg.ListenPort, PublicKey: publicKey}, nil
}

func (t *TransferService) resolvePeerForQueueItem(sender, senderIP string) (*Peer, error) {
	ip := strings.TrimSpace(senderIP)
	if ip == "" {
		return nil, errors.New("missing sender ip")
	}
	if t.discovery != nil {
		if peer, err := t.discovery.Resolve(ip); err == nil {
			return peer, nil
		}
	}
	if t.discovery == nil {
		return nil, errors.New("discovery service unavailable")
	}
	publicKey, ok := t.discovery.SharedPublicKey(sender, ip)
	if !ok || strings.TrimSpace(publicKey) == "" {
		return nil, fmt.Errorf("peer key unavailable for %s (%s)", sender, ip)
	}
	return &Peer{Username: sender, IP: ip, Port: t.cfg.ListenPort, PublicKey: publicKey}, nil
}

func (t *TransferService) registerPendingAck(key string) chan *envelope {
	ch := make(chan *envelope, 1)
	t.pendingAckMu.Lock()
	t.pendingAcks[key] = ch
	t.pendingAckMu.Unlock()
	return ch
}

func (t *TransferService) unregisterPendingAck(key string) {
	t.pendingAckMu.Lock()
	delete(t.pendingAcks, key)
	t.pendingAckMu.Unlock()
}

func (t *TransferService) notifyPendingAck(env *envelope) {
	peer := strings.TrimSpace(env.From)
	if peer == "" {
		peer = strings.TrimSpace(env.FromIP)
	}
	key := deliveryAckKey(env.AckKind, transferDisplayName(env.AckKind, env.Name), peer)
	t.pendingAckMu.Lock()
	ch := t.pendingAcks[key]
	t.pendingAckMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- env:
	default:
	}
}

func (t *TransferService) waitPendingAck(ch chan *envelope, timeout time.Duration) *envelope {
	if ch == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ack, ok := <-ch:
		if !ok {
			return nil
		}
		return ack
	case <-timer.C:
		return nil
	}
}

// humanizeTransferError maps low-level transport and OS errors to
// user-friendly explanations. Wherever possible it pattern-matches on
// typed errors (errors.Is) rather than on stringified messages — string
// pattern-matching is brittle across Go versions and locales.
//
// The substring fallbacks at the bottom catch errors that are wrapped as
// plain *errors.errorString (e.g. coming from third-party libraries or
// from our own historic `errors.New(...)` sites) and will be retired as
// those call sites move to typed errors.
func humanizeTransferError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "peer is not reachable (connection refused) — check they are running Bonjou on the same network"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "peer is not reachable — check your network connection"
	case errors.Is(err, syscall.EPIPE), errors.Is(err, net.ErrClosed):
		return "connection was lost mid-transfer — please try again"
	case errors.Is(err, syscall.ECONNRESET):
		return "peer closed the connection unexpectedly — please try again"
	case errors.Is(err, os.ErrNotExist):
		return "file no longer exists at the specified path"
	case errors.Is(err, os.ErrPermission):
		return "permission denied — check file and folder permissions"
	case errors.Is(err, context.DeadlineExceeded):
		return "connection timed out — check the peer is still reachable"
	}
	// Net error helper covers timeouts that don't surface a specific
	// errno (e.g. SetReadDeadline-driven net.OpError with a Timeout()).
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "connection timed out — check the peer is still reachable"
	}
	// Fallbacks for unwrapped/stringified errors from older code paths.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no delivery confirmation"):
		return "upload finished, but delivery could not be confirmed — the transfer may or may not have completed on the peer"
	case strings.Contains(msg, "integrity check failed"),
		strings.Contains(msg, "Delivery failed"):
		return msg
	default:
		return msg
	}
}

func (t *TransferService) emitTransferIssue(kind, name, peer, path string, err error) {
	if err == nil {
		return
	}
	label := strings.TrimSpace(name)
	if label == "" {
		label = "payload"
	}
	who := strings.TrimSpace(peer)
	if who == "" {
		who = "peer"
	}
	message := humanizeTransferError(err)
	if message == "" {
		message = err.Error()
	}
	t.emit(events.Event{
		Type:      events.Error,
		Title:     "Transfer failed",
		Message:   fmt.Sprintf("Failed to send %s '%s' to %s: %s", strings.ToLower(transferKindLabel(kind)), label, who, message),
		To:        who,
		Path:      path,
		Timestamp: time.Now(),
	})
}

func writeEnvelope(conn net.Conn, data []byte) error {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return err
	}
	return nil
}

func readEnvelope(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 {
		return nil, errors.New("empty envelope")
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(conn, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// readSecureEnvelope reads the next sealed frame from conn and decrypts it.
// It returns the inner envelope, the derived shared key (used for inner
// HMAC verification by the caller), the wire nonce of the sealed envelope
// (used for replay detection), and the peer public key the frame was opened
// with (used to bucket the replay cache).
func (t *TransferService) readSecureEnvelope(conn net.Conn) (*envelope, []byte, []byte, string, error) {
	frame, err := readEnvelope(conn)
	if err != nil {
		return nil, nil, nil, "", err
	}
	remoteIPs := remoteIPCandidates(conn)
	if len(remoteIPs) == 0 {
		return nil, nil, nil, "", errors.New("unable to resolve remote address")
	}
	if t.discovery == nil {
		return nil, nil, nil, "", errors.New("discovery service unavailable")
	}

	var lastKey []byte
	var lastPub string
	for _, ip := range remoteIPs {
		publicKey, ok := t.discovery.SharedPublicKey("", ip)
		if !ok || strings.TrimSpace(publicKey) == "" {
			continue
		}
		key, err := t.sharedKey(publicKey)
		if err != nil {
			continue
		}
		env, nonce, err := openEnvelope(frame, key)
		if err == nil {
			return env, key, nonce, publicKey, nil
		}
		lastKey = key
		lastPub = publicKey
	}
	if lastKey == nil {
		return nil, nil, nil, "", errors.New("unable to derive shared key")
	}

	env, nonce, err := openEnvelope(frame, lastKey)
	if err != nil {
		return nil, nil, nil, "", err
	}
	return env, lastKey, nonce, lastPub, nil
}

// sealEnvelope encrypts and authenticates the envelope under AES-256-GCM.
// The GCM tag bound to the ciphertext authenticates every byte of the
// envelope JSON, so the previous separate "HMAC over fields" pass is
// redundant and has been removed. Wire-format v2 ("v": 2).
func sealEnvelope(env *envelope, shared []byte) ([]byte, error) {
	plain, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	encKey, err := deriveEnvelopeKey(shared)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(encKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// AAD includes the wire version so a downgrade attack (peer rewrites v=2
	// to v=1 hoping we'll accept) is rejected at decryption time.
	aad := []byte(fmt.Sprintf("bonjou.v%d", sealedEnvelopeVersion))
	ciphertext := aead.Seal(nil, nonce, plain, aad)
	sealed := sealedEnvelope{
		Version: sealedEnvelopeVersion,
		Nonce:   hex.EncodeToString(nonce),
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
	}
	return json.Marshal(sealed)
}

// openEnvelope unwraps a sealed wire frame using AES-256-GCM and returns
// the inner envelope. The GCM tag both decrypts and authenticates, so any
// tampering with the version, nonce, ciphertext, or AAD is detected here.
// The wire nonce is returned so the caller can use it for replay detection.
func openEnvelope(frame []byte, shared []byte) (*envelope, []byte, error) {
	var sealed sealedEnvelope
	if err := json.Unmarshal(frame, &sealed); err != nil {
		return nil, nil, err
	}
	if sealed.Version != sealedEnvelopeVersion {
		return nil, nil, fmt.Errorf("unsupported envelope version: %d (need %d) — peer may be running an older Bonjou", sealed.Version, sealedEnvelopeVersion)
	}
	nonce, err := hex.DecodeString(sealed.Nonce)
	if err != nil {
		return nil, nil, fmt.Errorf("decode envelope nonce: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(sealed.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("decode envelope payload: %w", err)
	}
	encKey, err := deriveEnvelopeKey(shared)
	if err != nil {
		return nil, nil, err
	}
	aead, err := newGCM(encKey)
	if err != nil {
		return nil, nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, nil, fmt.Errorf("envelope nonce length = %d, want %d", len(nonce), aead.NonceSize())
	}
	aad := []byte(fmt.Sprintf("bonjou.v%d", sealedEnvelopeVersion))
	plain, err := aead.Open(nil, nonce, payload, aad)
	if err != nil {
		return nil, nil, fmt.Errorf("envelope authentication failed: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return nil, nil, err
	}
	return &env, nonce, nil
}

func randomHexID(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (t *TransferService) sharedKey(remotePublicKey string) ([]byte, error) {
	localKey, err := privateKeyFromSecret(t.cfg.Secret)
	if err != nil {
		return nil, err
	}
	remoteBytes, err := hex.DecodeString(strings.TrimSpace(remotePublicKey))
	if err != nil {
		return nil, err
	}
	curve := ecdh.X25519()
	remoteKey, err := curve.NewPublicKey(remoteBytes)
	if err != nil {
		return nil, err
	}
	shared, err := localKey.ECDH(remoteKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(shared)
	return sum[:], nil
}

func (t *TransferService) identity() (string, string) {
	t.localMu.RLock()
	defer t.localMu.RUnlock()
	return t.localUser, t.localIP
}

// UpdateLocalUser swaps the local username used for outgoing transfers.
func (t *TransferService) UpdateLocalUser(username string) {
	t.localMu.Lock()
	t.localUser = username
	t.localMu.Unlock()
}

// UpdateLocalEndpoint updates cached identity fields following network changes.
func (t *TransferService) UpdateLocalEndpoint(username, ip string) {
	t.localMu.Lock()
	if username != "" {
		t.localUser = username
	}
	if ip != "" {
		t.localIP = ip
	}
	t.localMu.Unlock()
}

func (t *TransferService) localUsername() string {
	t.localMu.RLock()
	defer t.localMu.RUnlock()
	return t.localUser
}

func (t *TransferService) storeOutgoingApproval(requestID string, approval *outgoingApproval) {
	t.outgoingMu.Lock()
	t.outgoingApprovals[requestID] = approval
	_ = t.saveOutgoingApprovalsLocked()
	t.outgoingMu.Unlock()
}

func (t *TransferService) getOutgoingApproval(requestID string) (*outgoingApproval, bool) {
	t.outgoingMu.Lock()
	defer t.outgoingMu.Unlock()
	approval, ok := t.outgoingApprovals[requestID]
	if !ok {
		return nil, false
	}
	copy := *approval
	return &copy, true
}

func (t *TransferService) deleteOutgoingApproval(requestID string) {
	t.outgoingMu.Lock()
	delete(t.outgoingApprovals, requestID)
	_ = t.saveOutgoingApprovalsLocked()
	t.outgoingMu.Unlock()
}

func (t *TransferService) loadOutgoingApprovals() {
	if err := os.MkdirAll(filepath.Dir(t.outgoingSnapshotPath), 0o755); err != nil {
		t.logger.Error("prepare outgoing approval directory: %v", err)
		return
	}

	data, err := os.ReadFile(t.outgoingSnapshotPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.logger.Error("read outgoing approvals: %v", err)
		}
		return
	}

	var snapshot map[string]*outgoingApproval
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.logger.Error("decode outgoing approvals: %v", err)
		return
	}

	t.outgoingMu.Lock()
	defer t.outgoingMu.Unlock()

	for requestID, approval := range snapshot {
		if approval == nil {
			continue
		}
		if strings.TrimSpace(requestID) == "" || strings.TrimSpace(approval.Path) == "" {
			continue
		}
		info, err := os.Stat(approval.Path)
		if err != nil {
			continue
		}
		switch approval.Kind {
		case kindFile:
			if info.IsDir() {
				continue
			}
		case kindFolder:
			if !info.IsDir() {
				continue
			}
		default:
			continue
		}
		copy := *approval
		t.outgoingApprovals[requestID] = &copy
	}
}

func (t *TransferService) saveOutgoingApprovalsLocked() error {
	if err := os.MkdirAll(filepath.Dir(t.outgoingSnapshotPath), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(t.outgoingApprovals, "", "  ")
	if err != nil {
		return err
	}

	tempPath := t.outgoingSnapshotPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tempPath, t.outgoingSnapshotPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func (t *TransferService) beginCancelableOperation() (<-chan struct{}, func()) {
	t.cancelMu.Lock()
	cancel := make(chan struct{})
	t.cancelActive = cancel
	t.cancelMu.Unlock()

	return cancel, func() {
		t.cancelMu.Lock()
		if t.cancelActive == cancel {
			t.cancelActive = nil
		}
		t.cancelMu.Unlock()
	}
}

func (t *TransferService) CancelActiveOperation() bool {
	t.cancelMu.Lock()
	defer t.cancelMu.Unlock()
	if t.cancelActive == nil {
		return false
	}
	close(t.cancelActive)
	t.cancelActive = nil
	return true
}

func (t *TransferService) checkCanceled(cancel <-chan struct{}) error {
	if cancel == nil {
		return nil
	}
	select {
	case <-cancel:
		return errors.New("operation cancelled")
	default:
		return nil
	}
}

// validateIncomingSize rejects payloads that exceed the configured maximum.
// This guards against a malicious sender claiming env.Size = math.MaxInt64
// (which would otherwise cause the receive loop to read forever) or simply
// making us allocate and write tens of TB to fill the disk.
func (t *TransferService) validateIncomingSize(size int64, label string) error {
	if size < 0 {
		return fmt.Errorf("rejected %s: declared size is negative (%d)", label, size)
	}
	if t == nil || t.cfg == nil {
		return nil
	}
	limit := t.cfg.MaxIncomingBytes
	if limit <= 0 {
		return nil
	}
	if size > limit {
		return fmt.Errorf(
			"rejected %s: declared size %s exceeds configured max %s — raise max_incoming_bytes in config to accept",
			label,
			formatSize(size),
			formatSize(limit),
		)
	}
	return nil
}

func ensurePathWithinRoot(path, root string) error {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path traversal not allowed: %s escapes %s", cleanPath, cleanRoot)
	}
	return nil
}

func formatSize(size int64) string {
	return format.Size(size)
}
