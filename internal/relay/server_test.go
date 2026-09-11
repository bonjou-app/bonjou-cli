package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerHasNoTransferDataPlane(t *testing.T) {
	s := NewServer(Options{})
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/t/transfer-id"},
		{http.MethodPost, "/t/transfer-id/0"},
		{http.MethodPost, "/t/transfer-id/end"},
	} {
		r := httptest.NewRequest(request.method, request.path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", request.method, request.path, w.Code)
		}
	}
}

func TestHealthDoesNotReportTransfers(t *testing.T) {
	s := NewServer(Options{})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if _, ok := body["transfers"]; ok {
		t.Fatalf("health response still exposes transfer state: %v", body)
	}
}

func TestClientIPPrefersProxySuppliedRealIP(t *testing.T) {
	s := NewServer(Options{TrustProxy: true})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.23")
	r.Header.Set("X-Real-IP", "198.51.100.23")

	if got, want := s.clientIP(r), "198.51.100.23"; got != want {
		t.Fatalf("clientIP() = %q, want %q", got, want)
	}
}

func TestClientIPUsesRightmostForwardedAddress(t *testing.T) {
	s := NewServer(Options{TrustProxy: true})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.23")

	if got, want := s.clientIP(r), "198.51.100.23"; got != want {
		t.Fatalf("clientIP() = %q, want %q", got, want)
	}
}

func TestClientIPIgnoresForwardedHeadersWithoutTrust(t *testing.T) {
	s := NewServer(Options{TrustProxy: false})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "192.0.2.44:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	r.Header.Set("X-Real-IP", "198.51.100.23")

	if got, want := s.clientIP(r), "192.0.2.44"; got != want {
		t.Fatalf("clientIP() = %q, want %q", got, want)
	}
}
