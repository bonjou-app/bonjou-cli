package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestClientIPExplicitTrustedHeader(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "IPv4", values: []string{"198.51.100.23"}, want: "198.51.100.23"},
		{name: "IPv6", values: []string{"2001:db8::23"}, want: "2001:db8::23"},
		{name: "canonical mapped IPv4", values: []string{"::ffff:198.51.100.23"}, want: "198.51.100.23"},
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "invalid", values: []string{"not-an-ip"}},
		{name: "forged prefix", values: []string{"203.0.113.99, 198.51.100.23"}},
		{name: "repeated", values: []string{"198.51.100.23", "203.0.113.99"}},
		{name: "port", values: []string{"198.51.100.23:443"}},
		{name: "scoped IPv6", values: []string{"fe80::1%eth0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(Options{TrustProxy: true, ClientIPHeader: "CF-Connecting-IP"})
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			r.RemoteAddr = "127.0.0.1:54321"
			r.Header.Set("X-Real-IP", "203.0.113.99")
			r.Header.Set("X-Forwarded-For", "203.0.113.99, 192.0.2.44")
			for _, value := range tt.values {
				r.Header.Add("CF-Connecting-IP", value)
			}
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got, tt.want)
			}
			if tt.want == "" {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("invalid trusted address status = %d, want 503", w.Code)
				}
				if s.hub.Rooms() != 0 {
					t.Fatal("invalid trusted address created coordinator state")
				}
			}
		})
	}
}

func TestExplicitClientIPHeaderRequiresProxyTrust(t *testing.T) {
	s := NewServer(Options{ClientIPHeader: "CF-Connecting-IP"})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("CF-Connecting-IP", "198.51.100.23")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("untrusted header configuration status = %d, want 503", w.Code)
	}
	if got := s.clientIP(r); got != "" {
		t.Fatalf("untrusted header returned address %q", got)
	}

	// Health probes do not have a browser's source address and must remain usable.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", w.Code)
	}
}

func TestExplicitTrustedHeaderPreservesNetworkIsolation(t *testing.T) {
	s := NewServer(Options{TrustProxy: true, ClientIPHeader: "CF-Connecting-IP"})
	address := func(ip string) string {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("CF-Connecting-IP", ip)
		// Other client-controlled header values must not change room membership.
		r.Header.Set("X-Real-IP", "198.51.100.23")
		r.Header.Set("X-Forwarded-For", "198.51.100.23, 192.0.2.44")
		return s.clientIP(r)
	}
	creator := testConn(t, s.hub, address("198.51.100.23"), strings.Repeat("a", pubKeyHexLen))
	if err := creator.handle(&clientMessage{Type: msgCreate}); err != nil {
		t.Fatalf("create: %v", err)
	}
	local := testConn(t, s.hub, address("198.51.100.23"), strings.Repeat("b", pubKeyHexLen))
	if err := local.handle(&clientMessage{Type: msgJoin, Code: creator.codeRoom.Code}); err != nil {
		t.Fatalf("same-network join: %v", err)
	}
	outside := testConn(t, s.hub, address("203.0.113.99"), strings.Repeat("c", pubKeyHexLen))
	if err := outside.handle(&clientMessage{Type: msgJoin, Code: creator.codeRoom.Code}); !errors.Is(err, errNetworkMatch) {
		t.Fatalf("different-network join error = %v, want errNetworkMatch", err)
	}
}
