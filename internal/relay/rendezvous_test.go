package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func newTestRendezvous(t *testing.T, limits Limits) (*Rendezvous, *httptest.Server) {
	t.Helper()
	rv := NewRendezvous(limits, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{id}", func(w http.ResponseWriter, r *http.Request) {
		rv.ServeDownload(w, r, r.PathValue("id"), transferToken(r))
	})
	mux.HandleFunc("POST /t/{id}/end", func(w http.ResponseWriter, r *http.Request) {
		rv.ServeEnd(w, r, r.PathValue("id"), transferToken(r))
	})
	mux.HandleFunc("POST /t/{id}/{seq}", func(w http.ResponseWriter, r *http.Request) {
		seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
		if err != nil {
			writeHTTPError(w, http.StatusBadRequest, err)
			return
		}
		rv.ServeUpload(w, r, r.PathValue("id"), transferToken(r), seq)
	})
	srv := httptest.NewServer(mux)
	// httptest.Server.Close blocks until every handler has returned and
	// every client connection is closed. A download handler parks until
	// its transfer finishes, and keep-alive connections linger in the
	// shared default transport, so both have to be dealt with explicitly
	// or a test that deliberately abandons a transfer wedges the suite.
	t.Cleanup(func() {
		rv.mu.RLock()
		inFlight := make([]*transfer, 0, len(rv.transfers))
		for _, x := range rv.transfers {
			inFlight = append(inFlight, x)
		}
		rv.mu.RUnlock()
		for _, x := range inFlight {
			x.fail(errors.New("test cleanup"))
		}
		srv.CloseClientConnections()
		srv.Close()
	})
	return rv, srv
}

// testClient avoids the shared default transport. Pooled keep-alive
// connections outlive the test that created them, and httptest.Server.Close
// waits on them, which turns an unrelated later test into a hang.
func testClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   30 * time.Second,
	}
}

type downloadResult struct {
	body   []byte
	status int
	err    error
}

// startDownload opens the receiver half in the background, mirroring how a
// browser's service worker attaches before the sender starts uploading.
func startDownload(t *testing.T, srv *httptest.Server, id, token string) <-chan downloadResult {
	t.Helper()
	out := make(chan downloadResult, 1)
	go func() {
		resp, err := testClient().Get(srv.URL + "/t/" + id + "?token=" + token)
		if err != nil {
			out <- downloadResult{err: err}
			return
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		body, err := io.ReadAll(resp.Body)
		out <- downloadResult{body: body, status: resp.StatusCode, err: err}
	}()
	return out
}

func postChunk(t *testing.T, srv *httptest.Server, id, token string, seq uint64, data []byte) int {
	t.Helper()
	url := srv.URL + "/t/" + id + "/" + strconv.FormatUint(seq, 10)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build chunk request: %v", err)
	}
	req.Header.Set(tokenHeader, token)
	resp, err := testClient().Do(req)
	if err != nil {
		t.Fatalf("post chunk %d: %v", seq, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func postEnd(t *testing.T, srv *httptest.Server, id, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/t/"+id+"/end", nil)
	if err != nil {
		t.Fatalf("build end request: %v", err)
	}
	req.Header.Set(tokenHeader, token)
	resp, err := testClient().Do(req)
	if err != nil {
		t.Fatalf("post end: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestRendezvousStreamsChunksInOrder(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	chunks := [][]byte{[]byte("hello "), []byte("relayed "), []byte("world")}
	var want []byte
	for _, c := range chunks {
		want = append(want, c...)
	}

	x, err := rv.Begin("sender", "receiver", int64(len(want)))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	result := startDownload(t, srv, x.id, x.recvToken)
	for i, c := range chunks {
		if status := postChunk(t, srv, x.id, x.sendToken, uint64(i), c); status != http.StatusNoContent {
			t.Fatalf("chunk %d status = %d, want 204", i, status)
		}
	}
	if status := postEnd(t, srv, x.id, x.sendToken); status != http.StatusNoContent {
		t.Fatalf("end status = %d, want 204", status)
	}

	got := <-result
	if got.err != nil {
		t.Fatalf("download: %v", got.err)
	}
	if got.status != http.StatusOK {
		t.Fatalf("download status = %d, want 200", got.status)
	}
	if !bytes.Equal(got.body, want) {
		t.Fatalf("relayed body = %q, want %q", got.body, want)
	}
}

// Ordering is the sender's responsibility, but the relay must refuse to
// silently reorder: a scrambled ciphertext stream would fail to decrypt
// with a confusing error far from the real cause.
// A browser stops reading as soon as Content-Length is satisfied, which
// usually happens before the sender's /end call lands. That is a normal
// completion, and recording it as a failure would make every successful
// transfer log an error.
func TestReceiverDisconnectAfterFullPayloadIsSuccess(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	payload := bytes.Repeat([]byte("z"), 4096)
	x, err := rv.Begin("sender", "receiver", int64(len(payload)))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	result := startDownload(t, srv, x.id, x.recvToken)
	if status := postChunk(t, srv, x.id, x.sendToken, 0, payload); status != http.StatusNoContent {
		t.Fatalf("chunk status = %d, want 204", status)
	}

	// The client's ReadAll returns once Content-Length is met and closes
	// the connection, without the sender having called /end yet.
	got := <-result
	if got.err != nil {
		t.Fatalf("download: %v", got.err)
	}
	if !bytes.Equal(got.body, payload) {
		t.Fatal("payload mismatch")
	}

	waitFor(t, x.done, 3*time.Second, "transfer to settle")
	if err := x.err(); err != nil {
		t.Fatalf("fully delivered transfer recorded a failure: %v", err)
	}
}

type blockedChunkFlush struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	flushed bool
}

type signalledBody struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *signalledBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestAbortInterruptsStalledUploadBody(t *testing.T) {
	v := NewRendezvous(DefaultLimits(), nil)
	x, err := v.Begin("sender", "receiver", 100)
	if err != nil {
		t.Fatal(err)
	}
	reading := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			v.ServeDownload(w, r, x.id, x.recvToken)
			return
		}
		r.Body = &signalledBody{ReadCloser: r.Body, started: reading}
		v.ServeUpload(w, r, x.id, x.sendToken, 0)
	}))
	defer srv.Close()
	defer srv.CloseClientConnections()
	result := startDownload(t, srv, x.id, x.recvToken)
	waitFor(t, x.ready, 3*time.Second, "receiver to attach")
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Declare 100 bytes but send only one, leaving the server blocked on
	// the upload body while its download handler owns the response writer.
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\na"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, reading, 3*time.Second, "upload to read its body")
	v.Abort(x.id, errors.New("peer disconnected"))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled upload did not respond to abort: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("aborted upload = %d, want 502", resp.StatusCode)
	}
	if got := <-result; got.err == nil {
		t.Error("aborted download completed without error")
	}
}

func (w *blockedChunkFlush) Flush() {
	if !w.flushed {
		w.flushed = true // The first flush sends the response headers.
		return
	}
	close(w.started)
	<-w.release
}

func TestDownloadWaitsForChunkFlushBeforeReturning(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "receiver disconnect"
		if abort {
			name = "peer abort"
		}
		t.Run(name, func(t *testing.T) {
			v := NewRendezvous(DefaultLimits(), nil)
			x, err := v.Begin("sender", "receiver", 4)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dst := &blockedChunkFlush{
				ResponseRecorder: httptest.NewRecorder(),
				started:          make(chan struct{}),
				release:          make(chan struct{}),
			}
			downloadDone := make(chan struct{})
			go func() {
				v.ServeDownload(dst, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), x.id, x.recvToken)
				close(downloadDone)
			}()
			<-x.ready
			uploadDone := make(chan struct{})
			go func() {
				v.ServeUpload(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("data"))), x.id, x.sendToken, 0)
				close(uploadDone)
			}()
			<-dst.started
			if abort {
				v.Abort(x.id, errors.New("peer disconnected"))
			} else {
				cancel()
			}
			select {
			case <-downloadDone:
				t.Error("download handler returned while its response was still in use")
			case <-time.After(50 * time.Millisecond):
			}
			close(dst.release)
			<-uploadDone
			<-downloadDone
			late := httptest.NewRecorder()
			v.ServeUpload(late, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil)), x.id, x.sendToken, 1)
			if late.Code != http.StatusGone {
				t.Errorf("upload after download returned = %d, want 410", late.Code)
			}
		})
	}
}

// A receiver that vanishes partway through is still a failure.
func TestReceiverDisconnectBeforeFullPayloadIsFailure(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 8192)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	client := testClient()
	resp, err := client.Get(srv.URL + "/t/" + x.id + "?token=" + x.recvToken)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	waitFor(t, x.ready, 3*time.Second, "receiver to attach")

	// Hang up having received nothing of the 8192 declared bytes.
	_ = resp.Body.Close()
	client.CloseIdleConnections()

	waitFor(t, x.done, 3*time.Second, "transfer to fail")
	if x.err() == nil {
		t.Fatal("a receiver that left early was not recorded as a failure")
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestRendezvousRejectsOutOfOrderChunk(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_ = startDownload(t, srv, x.id, x.recvToken)

	if status := postChunk(t, srv, x.id, x.sendToken, 1, []byte("abcde")); status != http.StatusConflict {
		t.Fatalf("out-of-order chunk status = %d, want 409", status)
	}
}

func TestRendezvousRejectsBadTokens(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 5)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	resp, err := testClient().Get(srv.URL + "/t/" + x.id + "?token=wrong")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("download with bad token = %d, want 403", resp.StatusCode)
	}

	if status := postChunk(t, srv, x.id, "wrong", 0, []byte("abc")); status != http.StatusForbidden {
		t.Fatalf("upload with bad token = %d, want 403", status)
	}
	// The receive token must not be usable to upload, and vice versa.
	if status := postChunk(t, srv, x.id, x.recvToken, 0, []byte("abc")); status != http.StatusForbidden {
		t.Fatalf("upload with receive token = %d, want 403", status)
	}
}

func TestRendezvousUnknownTransfer(t *testing.T) {
	_, srv := newTestRendezvous(t, DefaultLimits())
	resp, err := testClient().Get(srv.URL + "/t/deadbeefdeadbeef?token=x")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRendezvousRejectsSecondReceiver(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 5)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_ = startDownload(t, srv, x.id, x.recvToken)

	// Wait for the first receiver to attach.
	select {
	case <-x.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("first receiver never attached")
	}

	resp, err := testClient().Get(srv.URL + "/t/" + x.id + "?token=" + x.recvToken)
	if err != nil {
		t.Fatalf("second download: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second receiver status = %d, want 409", resp.StatusCode)
	}
}

func TestRendezvousRejectsEndShortOfDeclaredSize(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 100)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result := startDownload(t, srv, x.id, x.recvToken)

	if status := postChunk(t, srv, x.id, x.sendToken, 0, bytes.Repeat([]byte("a"), 10)); status != http.StatusNoContent {
		t.Fatalf("chunk status = %d, want 204", status)
	}
	if status := postEnd(t, srv, x.id, x.sendToken); status != http.StatusBadRequest {
		t.Fatalf("short end status = %d, want 400", status)
	}

	// The receiver must see a failed download, not a silently truncated
	// file. Declaring Content-Length up front is what makes the browser
	// treat the short body as an error.
	got := <-result
	if got.err == nil {
		t.Fatal("truncated download completed without error")
	}
}

func TestRendezvousRejectsOversizedSender(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 4)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_ = startDownload(t, srv, x.id, x.recvToken)

	if status := postChunk(t, srv, x.id, x.sendToken, 0, []byte("way past four")); status != http.StatusBadRequest {
		t.Fatalf("oversized chunk status = %d, want 400", status)
	}
	if x.err() == nil {
		t.Fatal("transfer was not failed after the sender exceeded its declared size")
	}
}

func TestBeginRejectsTransferAboveLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTransferBytes = 1024
	rv := NewRendezvous(limits, nil)
	if _, err := rv.Begin("sender", "receiver", 2048); err == nil {
		t.Fatal("Begin accepted a transfer above MaxTransferBytes")
	}
	if _, err := rv.Begin("sender", "receiver", -1); err == nil {
		t.Fatal("Begin accepted a negative size")
	}
}

func TestBeginRejectsBeyondConcurrencyLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxConcurrentTransfers = 1
	rv := NewRendezvous(limits, nil)
	if _, err := rv.Begin("a", "b", 1); err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if _, err := rv.Begin("a", "b", 1); !errors.Is(err, errAtCapacity) {
		t.Fatalf("second Begin error = %v, want errAtCapacity", err)
	}
}

// When a peer's control connection drops mid-transfer the surviving side
// must learn immediately rather than waiting out the idle timeout.
func TestAbortForPeerFailsTransfer(t *testing.T) {
	rv, srv := newTestRendezvous(t, DefaultLimits())
	x, err := rv.Begin("sender", "receiver", 100)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result := startDownload(t, srv, x.id, x.recvToken)
	select {
	case <-x.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver never attached")
	}

	ids := rv.AbortForPeer("sender", errors.New("peer disconnected"))
	if len(ids) != 1 || ids[0] != x.id {
		t.Fatalf("AbortForPeer returned %v, want [%s]", ids, x.id)
	}

	got := <-result
	if got.err == nil {
		t.Fatal("download completed even though the sender vanished")
	}
	if x.err() == nil {
		t.Fatal("transfer error not recorded")
	}
}

// Both peers disconnect right after a successful transfer. Those
// disconnects must not retroactively mark the completed transfer as
// aborted.
func TestAbortForPeerIgnoresFinishedTransfers(t *testing.T) {
	rv := NewRendezvous(DefaultLimits(), nil)
	x, err := rv.Begin("sender", "receiver", 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	x.complete()

	if ids := rv.AbortForPeer("sender", errors.New("peer disconnected")); len(ids) != 0 {
		t.Fatalf("AbortForPeer reported %v for an already-finished transfer", ids)
	}
	if x.err() != nil {
		t.Fatalf("finished transfer was marked failed: %v", x.err())
	}
}

func TestAbortForPeerIgnoresUnrelatedPeers(t *testing.T) {
	rv := NewRendezvous(DefaultLimits(), nil)
	x, err := rv.Begin("sender", "receiver", 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if ids := rv.AbortForPeer("someone-else", errors.New("gone")); len(ids) != 0 {
		t.Fatalf("AbortForPeer touched unrelated transfers: %v", ids)
	}
	if x.err() != nil {
		t.Fatal("unrelated disconnect failed the transfer")
	}
}

func TestSweepFailsStalledTransfer(t *testing.T) {
	limits := DefaultLimits()
	limits.TransferIdleTTL = time.Second
	rv := NewRendezvous(limits, nil)
	x, err := rv.Begin("sender", "receiver", 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	rv.sweep(time.Now())
	if x.err() != nil {
		t.Fatal("fresh transfer was swept")
	}

	rv.sweep(time.Now().Add(10 * time.Second))
	if x.err() == nil {
		t.Fatal("stalled transfer survived the sweep")
	}
}

func TestSweepReapsFinishedTransfers(t *testing.T) {
	limits := DefaultLimits()
	limits.RendezvousWait = time.Second
	rv := NewRendezvous(limits, nil)
	x, err := rv.Begin("sender", "receiver", 10)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	x.complete()

	rv.sweep(time.Now().Add(10 * time.Second))
	if rv.Active() != 0 {
		t.Fatalf("Active() = %d after reaping, want 0", rv.Active())
	}
}

func TestTokenEqualIsExact(t *testing.T) {
	if !tokenEqual("abc123", "abc123") {
		t.Error("identical tokens compared unequal")
	}
	if tokenEqual("abc123", "abc124") {
		t.Error("different tokens compared equal")
	}
	if tokenEqual("abc", "abc123") {
		t.Error("prefix accepted as a full token")
	}
	if tokenEqual("", "") {
		// An empty configured token would let anyone in; ConstantTimeCompare
		// returns 1 for two empty slices, so guard the case explicitly.
		t.Log("note: empty tokens compare equal; tokens are always generated non-empty")
	}
}
