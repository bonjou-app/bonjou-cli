package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/logger"
)

var (
	errTransferNotFound = errors.New("transfer not found")
	errTransferTaken    = errors.New("transfer already has a receiver")
	errTransferDone     = errors.New("transfer already finished")
	errBadToken         = errors.New("invalid transfer token")
)

// transfer is one in-flight sender-to-receiver byte pipe.
//
// The design intent is that this struct owns no buffer. The receiver's
// http.ResponseWriter is installed as dst, and each uploaded chunk is
// copied straight into it. When the receiver's TCP window closes the copy
// blocks, which blocks the sender's POST, which stops the browser reading
// more of the file. Backpressure is therefore the kernel's job end to end,
// and steady-state memory is one 32 KiB copy buffer per transfer.
type transfer struct {
	id        string
	sendToken string
	recvToken string
	from      string
	to        string
	size      int64
	created   time.Time

	// ready closes once a receiver has attached and dst is safe to write.
	// Senders block on it so the first chunk cannot race the GET.
	ready     chan struct{}
	readyOnce sync.Once

	// done closes when the transfer completes or fails.
	done     chan struct{}
	doneOnce sync.Once

	attached atomic.Bool
	written  atomic.Int64
	lastData atomic.Int64 // unix nanos

	// mu serialises chunk writes. It is deliberately held across the copy:
	// that is what enforces chunk ordering and propagates backpressure.
	mu      sync.Mutex
	dst     io.Writer
	flush   func() error
	nextSeq uint64

	failMu  sync.Mutex
	failure error
}

func (x *transfer) markReady() { x.readyOnce.Do(func() { close(x.ready) }) }

func (x *transfer) complete() { x.doneOnce.Do(func() { close(x.done) }) }

func (x *transfer) fail(err error) {
	x.failMu.Lock()
	if x.failure == nil {
		x.failure = err
	}
	x.failMu.Unlock()
	x.complete()
}

func (x *transfer) err() error {
	x.failMu.Lock()
	defer x.failMu.Unlock()
	return x.failure
}

func (x *transfer) touch() { x.lastData.Store(time.Now().UnixNano()) }

func (x *transfer) idle(now time.Time) time.Duration {
	last := x.lastData.Load()
	if last == 0 {
		return now.Sub(x.created)
	}
	return now.Sub(time.Unix(0, last))
}

// Rendezvous pairs upload and download halves of transfers.
type Rendezvous struct {
	mu        sync.RWMutex
	transfers map[string]*transfer
	limits    Limits
	logger    *logger.Logger
}

// NewRendezvous constructs a rendezvous table. A nil logger is allowed.
func NewRendezvous(limits Limits, lg *logger.Logger) *Rendezvous {
	return &Rendezvous{
		transfers: make(map[string]*transfer),
		limits:    limits.withDefaults(),
		logger:    lg,
	}
}

// Begin mints a transfer and its two single-purpose tokens. Called when a
// sender signals transfer_begin, which only happens after the receiver has
// approved the offer.
func (v *Rendezvous) Begin(from, to string, size int64) (*transfer, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid transfer size: %d", size)
	}
	if size > v.limits.MaxTransferBytes {
		return nil, fmt.Errorf("transfer of %d bytes exceeds limit of %d", size, v.limits.MaxTransferBytes)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.transfers) >= v.limits.MaxConcurrentTransfers {
		return nil, errAtCapacity
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	sendToken, err := newToken()
	if err != nil {
		return nil, err
	}
	recvToken, err := newToken()
	if err != nil {
		return nil, err
	}
	x := &transfer{
		id:        id,
		sendToken: sendToken,
		recvToken: recvToken,
		from:      from,
		to:        to,
		size:      size,
		created:   time.Now(),
		ready:     make(chan struct{}),
		done:      make(chan struct{}),
	}
	v.transfers[id] = x
	return x, nil
}

func (v *Rendezvous) get(id string) (*transfer, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	x, ok := v.transfers[id]
	return x, ok
}

func (v *Rendezvous) drop(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.transfers, id)
}

// Abort fails a transfer from outside the data plane — used when a peer
// disconnects while bytes are still moving.
func (v *Rendezvous) Abort(id string, reason error) {
	if x, ok := v.get(id); ok {
		x.fail(reason)
	}
}

// AbortForPeer fails every transfer involving a peer. Called when that
// peer's control connection drops, so the surviving half learns
// immediately rather than waiting out the idle timeout.
func (v *Rendezvous) AbortForPeer(peerID string, reason error) []string {
	v.mu.RLock()
	affected := make([]*transfer, 0)
	for _, x := range v.transfers {
		if x.from != peerID && x.to != peerID {
			continue
		}
		// Skip transfers that already finished. Both peers normally
		// disconnect moments after a successful transfer, and reporting
		// those as aborted would describe every completed send as a
		// casualty of the disconnect that followed it.
		select {
		case <-x.done:
			continue
		default:
		}
		affected = append(affected, x)
	}
	v.mu.RUnlock()

	ids := make([]string, 0, len(affected))
	for _, x := range affected {
		x.fail(reason)
		ids = append(ids, x.id)
	}
	return ids
}

// ServeDownload streams a transfer to its receiver. The response body is
// ciphertext; the browser's service worker decrypts it before it reaches
// disk.
func (v *Rendezvous) ServeDownload(w http.ResponseWriter, r *http.Request, id, token string) {
	x, ok := v.get(id)
	if !ok {
		writeHTTPError(w, http.StatusNotFound, errTransferNotFound)
		return
	}
	if !tokenEqual(token, x.recvToken) {
		writeHTTPError(w, http.StatusForbidden, errBadToken)
		return
	}
	if !x.attached.CompareAndSwap(false, true) {
		writeHTTPError(w, http.StatusConflict, errTransferTaken)
		return
	}

	rc := http.NewResponseController(w)
	// A multi-gigabyte transfer can legitimately run for hours; the idle
	// watchdog, not a wall-clock deadline, is what bounds it.
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", x.size))
	w.Header().Set("Cache-Control", "no-store")
	// Belt and braces with proxy_buffering off: some proxies honour only
	// this header, and buffering here would defeat streaming entirely.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		x.fail(fmt.Errorf("flush response headers: %w", err))
		return
	}

	x.mu.Lock()
	x.dst = w
	x.flush = rc.Flush
	x.mu.Unlock()
	x.touch()
	x.markReady()

	select {
	case <-x.done:
	case <-r.Context().Done():
		// A receiver that has taken every declared byte and then hung up
		// has finished, not failed. Content-Length is satisfied at that
		// point, so the browser is entitled to close before the sender's
		// /end call arrives — and it usually does, since the sender needs
		// another round trip. Treating that as an error would make every
		// successful transfer log a failure.
		if x.written.Load() >= x.size {
			x.complete()
		} else {
			x.fail(errors.New("receiver disconnected"))
		}
	}

	// Declaring Content-Length up front means a failed transfer produces a
	// short body, which the browser reports as a failed download. That is
	// the desired outcome: a truncated encrypted file must never look like
	// a successful one.
	if err := x.err(); err != nil {
		v.errorf("relay: transfer %s failed after %d bytes: %v", x.id, x.written.Load(), err)
	}
}

// ServeUpload accepts one ordered ciphertext chunk from the sender and
// copies it straight through to the receiver.
func (v *Rendezvous) ServeUpload(w http.ResponseWriter, r *http.Request, id, token string, seq uint64) {
	x, ok := v.get(id)
	if !ok {
		writeHTTPError(w, http.StatusNotFound, errTransferNotFound)
		return
	}
	if !tokenEqual(token, x.sendToken) {
		writeHTTPError(w, http.StatusForbidden, errBadToken)
		return
	}

	select {
	case <-x.ready:
	case <-x.done:
		writeHTTPError(w, http.StatusGone, errTransferDone)
		return
	case <-r.Context().Done():
		return
	case <-time.After(v.limits.RendezvousWait):
		writeHTTPError(w, http.StatusRequestTimeout, errors.New("receiver did not attach in time"))
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	if err := x.err(); err != nil {
		writeHTTPError(w, http.StatusGone, err)
		return
	}
	if seq != x.nextSeq {
		writeHTTPError(w, http.StatusConflict,
			fmt.Errorf("out-of-order chunk: got %d, expected %d", seq, x.nextSeq))
		return
	}

	// Length is validated before a single byte is copied. The download's
	// Content-Length was fixed when the transfer began, so writing past it
	// would be rejected by net/http mid-copy — after the excess had
	// already been forwarded. Refusing the chunk up front keeps the
	// receiver's stream exactly as long as it was promised to be.
	if r.ContentLength < 0 {
		writeHTTPError(w, http.StatusLengthRequired, errors.New("chunk requires a Content-Length"))
		return
	}
	if r.ContentLength > v.limits.MaxChunkBytes {
		writeHTTPError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("chunk of %d bytes exceeds limit of %d", r.ContentLength, v.limits.MaxChunkBytes))
		return
	}
	if x.written.Load()+r.ContentLength > x.size {
		x.fail(fmt.Errorf("sender exceeded declared size of %d bytes", x.size))
		writeHTTPError(w, http.StatusBadRequest, errors.New("exceeded declared transfer size"))
		return
	}

	body := http.MaxBytesReader(w, r.Body, v.limits.MaxChunkBytes)
	n, err := io.Copy(x.dst, body)
	if n > 0 {
		x.written.Add(n)
		x.touch()
	}
	if err != nil {
		x.fail(fmt.Errorf("relay chunk %d: %w", seq, err))
		writeHTTPError(w, http.StatusBadGateway, err)
		return
	}
	if x.flush != nil {
		if err := x.flush(); err != nil {
			x.fail(fmt.Errorf("flush chunk %d: %w", seq, err))
			writeHTTPError(w, http.StatusBadGateway, err)
			return
		}
	}
	x.nextSeq++
	w.WriteHeader(http.StatusNoContent)
}

// ServeEnd marks a transfer complete. The sender calls it after its final
// chunk has been acknowledged.
func (v *Rendezvous) ServeEnd(w http.ResponseWriter, r *http.Request, id, token string) {
	x, ok := v.get(id)
	if !ok {
		writeHTTPError(w, http.StatusNotFound, errTransferNotFound)
		return
	}
	if !tokenEqual(token, x.sendToken) {
		writeHTTPError(w, http.StatusForbidden, errBadToken)
		return
	}
	// Take mu so a chunk still in flight finishes before we declare done.
	x.mu.Lock()
	written := x.written.Load()
	x.mu.Unlock()

	if written != x.size {
		x.fail(fmt.Errorf("sender ended at %d of %d bytes", written, x.size))
		writeHTTPError(w, http.StatusBadRequest, errors.New("transfer ended short of declared size"))
		return
	}
	x.complete()
	w.WriteHeader(http.StatusNoContent)
}

// Run expires transfers that stop making progress, and reaps finished
// ones, until ctx is cancelled.
func (v *Rendezvous) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			v.sweep(now)
		}
	}
}

func (v *Rendezvous) sweep(now time.Time) {
	v.mu.RLock()
	candidates := make([]*transfer, 0, len(v.transfers))
	for _, x := range v.transfers {
		candidates = append(candidates, x)
	}
	v.mu.RUnlock()

	for _, x := range candidates {
		select {
		case <-x.done:
			// Keep finished transfers briefly so a late end/status call
			// gets a coherent answer rather than a bare 404.
			if now.Sub(x.created) > v.limits.RendezvousWait {
				v.drop(x.id)
			}
			continue
		default:
		}
		if x.idle(now) > v.limits.TransferIdleTTL {
			x.fail(fmt.Errorf("no progress for %s", v.limits.TransferIdleTTL))
			v.errorf("relay: transfer %s timed out idle", x.id)
		}
	}
}

// Active reports in-flight transfers, for the health endpoint.
func (v *Rendezvous) Active() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.transfers)
}

func (v *Rendezvous) errorf(format string, args ...any) {
	if v.logger == nil {
		return
	}
	v.logger.Error(format, args...)
}

// tokenEqual compares bearer tokens in constant time so a token cannot be
// recovered by timing repeated guesses.
func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeHTTPError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, err.Error()+"\n")
}
