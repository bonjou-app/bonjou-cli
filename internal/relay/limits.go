package relay

import (
	"sync"
	"time"
)

// Limits bounds what a single client, or the internet at large, can make
// the coordinator do. Every field has a defensive default; zero values are
// replaced by DefaultLimits at construction.
type Limits struct {
	// MaxRooms caps concurrent rooms process-wide.
	MaxRooms int
	// MaxPeersPerRoom caps an explicitly joined room.
	MaxPeersPerRoom int
	// MaxNetworkPeers caps an auto-grouped candidate set. Candidates do not
	// become visible until a host-only WebRTC connection proves that they can
	// reach one another directly on the local network.
	MaxNetworkPeers int
	// RoomIdleTTL is how long a room survives with no traffic.
	RoomIdleTTL time.Duration
	// CreatePerIPPerMin throttles room creation per source address.
	CreatePerIPPerMin int
}

// DefaultLimits returns production defaults sized for a 4-core, 24 GB box.
func DefaultLimits() Limits {
	return Limits{
		MaxRooms:          5000,
		MaxPeersPerRoom:   128,
		MaxNetworkPeers:   128,
		RoomIdleTTL:       30 * time.Minute,
		CreatePerIPPerMin: 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxRooms <= 0 {
		l.MaxRooms = d.MaxRooms
	}
	if l.MaxPeersPerRoom <= 0 {
		l.MaxPeersPerRoom = d.MaxPeersPerRoom
	}
	if l.MaxNetworkPeers <= 0 {
		l.MaxNetworkPeers = d.MaxNetworkPeers
	}
	if l.RoomIdleTTL <= 0 {
		l.RoomIdleTTL = d.RoomIdleTTL
	}
	if l.CreatePerIPPerMin <= 0 {
		l.CreatePerIPPerMin = d.CreatePerIPPerMin
	}
	return l
}

// rateLimiter is a fixed-window counter keyed by source address. A fixed
// window is coarser than a token bucket at the boundary, but the thing it
// guards — room creation — is cheap enough that burst precision does not
// matter, and the simpler structure has less to get wrong.
type rateLimiter struct {
	mu      sync.Mutex
	perMin  int
	windows map[string]*rateWindow
}

type rateWindow struct {
	count int
	start time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: perMin, windows: make(map[string]*rateWindow)}
}

// allow reports whether key may perform another action now.
func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	w, ok := rl.windows[key]
	if !ok || now.Sub(w.start) >= time.Minute {
		rl.windows[key] = &rateWindow{count: 1, start: now}
		return true
	}
	if w.count >= rl.perMin {
		return false
	}
	w.count++
	return true
}

// sweep drops windows that have aged out, so the map cannot grow without
// bound as source addresses churn.
func (rl *rateLimiter) sweep(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for key, w := range rl.windows {
		if now.Sub(w.start) >= 2*time.Minute {
			delete(rl.windows, key)
		}
	}
}
