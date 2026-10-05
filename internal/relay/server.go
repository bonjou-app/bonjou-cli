package relay

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/bonjou-app/bonjou-cli/internal/logger"
)

// Options configures a coordinator server.
type Options struct {
	Limits Limits
	Logger *logger.Logger
	// AllowedOrigins lists browser origins permitted to reach the coordinator,
	// e.g. "https://bonjou.vercel.app". A single "*" allows any origin.
	AllowedOrigins []string
	// TrustProxy makes the coordinator read the client address from
	// X-Real-IP / X-Forwarded-For. Correct behind nginx; must stay false
	// if the coordinator is ever exposed directly, since otherwise a client can
	// forge the header and walk around per-IP rate limits.
	TrustProxy bool
	// ClientIPHeader selects one header overwritten by a trusted ingress, such
	// as CF-Connecting-IP on a public Render web service. It requires TrustProxy.
	// Missing, repeated, or invalid values fail closed instead of placing clients
	// from different networks into a shared proxy-address discovery group.
	// Leave empty for the packaged nginx X-Real-IP / X-Forwarded-For behavior.
	ClientIPHeader string
}

// Server exposes health, room membership, and encrypted WebRTC signaling.
type Server struct {
	hub            *Hub
	logger         *logger.Logger
	origins        []string
	allowAll       bool
	trustProxy     bool
	clientIPHeader string
	started        time.Time
}

// NewServer constructs a coordinator server.
func NewServer(opts Options) *Server {
	limits := opts.Limits.withDefaults()
	s := &Server{
		hub:            NewHub(limits, opts.Logger),
		logger:         opts.Logger,
		trustProxy:     opts.TrustProxy,
		clientIPHeader: strings.TrimSpace(opts.ClientIPHeader),
		started:        time.Now(),
	}
	for _, origin := range opts.AllowedOrigins {
		trimmed := strings.TrimSpace(strings.TrimSuffix(origin, "/"))
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			s.allowAll = true
			continue
		}
		s.origins = append(s.origins, strings.ToLower(trimmed))
	}
	return s
}

// Run starts background maintenance and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	s.hub.Run(ctx)
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	return s.withCORS(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"rooms":      s.hub.Rooms(),
		"uptime_sec": int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	clientIP := s.clientIP(r)
	if s.clientIPHeader != "" && clientIP == "" {
		http.Error(w, "trusted client address unavailable", http.StatusServiceUnavailable)
		return
	}
	opts := &websocket.AcceptOptions{}
	if s.allowAll {
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = s.originPatterns()
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.errorf("coordinator: websocket accept from %s: %v", clientIP, err)
		return
	}
	conn, err := newConn(ws, s.hub, clientIP)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "could not allocate peer id")
		return
	}
	defer func() {
		_ = ws.CloseNow()
	}()
	// The request context is cancelled once Accept hijacks the connection
	// on some stacks, so the read loop runs on its own background context.
	conn.run(context.Background())
}

// withCORS echoes an allowed origin back. The coordinator carries only
// candidate state and encrypted signaling, but origin checking still
// keeps a hostile page from quietly enumerating rooms in a visitor's
// browser.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	if s.allowAll {
		return true
	}
	candidate := strings.ToLower(strings.TrimSuffix(origin, "/"))
	for _, allowed := range s.origins {
		if candidate == allowed {
			return true
		}
	}
	return false
}

// originPatterns converts configured origins into the host patterns the
// websocket library matches against.
func (s *Server) originPatterns() []string {
	out := make([]string, 0, len(s.origins))
	for _, origin := range s.origins {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			out = append(out, u.Host)
			continue
		}
		out = append(out, origin)
	}
	return out
}

// clientIP resolves the address used for rate limiting and network membership.
// An empty address in explicit-header mode must be rejected before accepting
// a WebSocket; it must never fall back to a shared ingress address.
func (s *Server) clientIP(r *http.Request) string {
	if s.clientIPHeader != "" {
		if !s.trustProxy {
			return ""
		}
		values := r.Header.Values(s.clientIPHeader)
		if len(values) != 1 {
			return ""
		}
		ip := net.ParseIP(strings.TrimSpace(values[0]))
		if ip == nil {
			return ""
		}
		return ip.String()
	}
	if s.trustProxy {
		// The packaged nginx configuration overwrites X-Real-IP with the
		// address of its client, so prefer it over the client-controlled
		// beginning of an X-Forwarded-For chain.
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// proxy_add_x_forwarded_for appends the address seen by the trusted
			// proxy. The rightmost value is therefore the only safe fallback
			// when a caller supplied a forged prefix.
			parts := strings.Split(forwarded, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if candidate := strings.TrimSpace(parts[i]); candidate != "" {
					return candidate
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) errorf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Error(format, args...)
}
