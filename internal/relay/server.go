package relay

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/bonjou-app/bonjou-cli/internal/logger"
)

// tokenHeader carries a transfer's bearer token. The download half also
// accepts ?token= because a service worker fetch is easier to reason about
// with the token in the URL it already constructs.
const tokenHeader = "X-Bonjou-Token"

// Options configures a relay server.
type Options struct {
	Limits Limits
	Logger *logger.Logger
	// AllowedOrigins lists browser origins permitted to reach the relay,
	// e.g. "https://bonjou.vercel.app". A single "*" allows any origin.
	AllowedOrigins []string
	// TrustProxy makes the relay read the client address from
	// X-Forwarded-For / X-Real-IP. Correct behind nginx; must stay false
	// if the relay is ever exposed directly, since otherwise a client can
	// forge the header and walk around per-IP rate limits.
	TrustProxy bool
}

// Server wires the control plane and data plane onto one HTTP handler.
type Server struct {
	hub        *Hub
	rv         *Rendezvous
	logger     *logger.Logger
	origins    []string
	allowAll   bool
	trustProxy bool
	started    time.Time
}

// NewServer constructs a relay server.
func NewServer(opts Options) *Server {
	limits := opts.Limits.withDefaults()
	s := &Server{
		hub:        NewHub(limits, opts.Logger),
		rv:         NewRendezvous(limits, opts.Logger),
		logger:     opts.Logger,
		trustProxy: opts.TrustProxy,
		started:    time.Now(),
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
	go s.hub.Run(ctx)
	go s.rv.Run(ctx)
	<-ctx.Done()
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /t/{id}", s.handleDownload)
	mux.HandleFunc("POST /t/{id}/end", s.handleUploadEnd)
	mux.HandleFunc("POST /t/{id}/{seq}", s.handleUpload)
	mux.HandleFunc("OPTIONS /", s.handlePreflight)
	return s.withCORS(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"rooms":      s.hub.Rooms(),
		"transfers":  s.rv.Active(),
		"uptime_sec": int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if s.allowAll {
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = s.originPatterns()
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.errorf("relay: websocket accept from %s: %v", s.clientIP(r), err)
		return
	}
	conn, err := newConn(ws, s.hub, s.rv, s.clientIP(r))
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

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.rv.ServeDownload(w, r, r.PathValue("id"), transferToken(r))
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	s.rv.ServeUpload(w, r, r.PathValue("id"), transferToken(r), seq)
}

func (s *Server) handleUploadEnd(w http.ResponseWriter, r *http.Request) {
	s.rv.ServeEnd(w, r, r.PathValue("id"), transferToken(r))
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// withCORS echoes an allowed origin back. The relay carries only
// ciphertext and per-transfer bearer tokens, but origin checking still
// keeps a hostile page from quietly enumerating rooms in a visitor's
// browser.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", tokenHeader+", Content-Type")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
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

// clientIP resolves the address used for rate limiting.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if first, _, ok := strings.Cut(forwarded, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(forwarded)
		}
		if real := r.Header.Get("X-Real-IP"); real != "" {
			return strings.TrimSpace(real)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func transferToken(r *http.Request) string {
	if token := r.Header.Get(tokenHeader); token != "" {
		return token
	}
	return r.URL.Query().Get("token")
}

func (s *Server) errorf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Error(format, args...)
}
