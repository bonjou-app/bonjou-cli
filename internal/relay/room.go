package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/logger"
)

var (
	errRoomNotFound  = errors.New("no room with that code — it may have expired")
	errRoomFull      = errors.New("room is full")
	errAtCapacity    = errors.New("coordinator is at capacity, try again shortly")
	errPeerNotFound  = errors.New("that peer is no longer reachable")
	errRateLimited   = errors.New("too many rooms created from this address")
	errAlreadyInRoom = errors.New("this connection is already in a room")
	errNotInRoom     = errors.New("say hello first")
	errNetworkBusy   = errors.New("too many devices share this network address to group them safely")
	errNetworkMatch  = errors.New("that room belongs to a different network")
	errUnsupported   = errors.New("message type is no longer supported")
)

// peerSendBuffer is how many control frames may queue for one client
// before the coordinator gives up on it. Control frames are small and rare,
// consisting only of roster updates and encrypted WebRTC signaling, so a
// client that falls this far
// behind is not reading its socket, and disconnecting it is kinder than
// growing a queue forever.
const peerSendBuffer = 64

// Room kinds. A network room is joined automatically by everyone sharing
// a source network; a code room is entered deliberately via a short code.
const (
	roomKindNetwork = "network"
	roomKindCode    = "code"
)

// Peer is one connected browser. A peer belongs to its network room and,
// optionally, to a code room; what it can see is the union of both.
type Peer struct {
	ID     string
	PubKey string

	send     chan *serverMessage
	closed   chan struct{}
	closeOne sync.Once

	mu    sync.RWMutex
	rooms map[string]*Room
}

func newPeer(id, pubKey string) *Peer {
	return &Peer{
		ID:     id,
		PubKey: pubKey,
		send:   make(chan *serverMessage, peerSendBuffer),
		closed: make(chan struct{}),
		rooms:  make(map[string]*Room),
	}
}

// Send queues a frame for delivery. It never blocks: if the peer's queue
// is full the peer is closed, because a client that cannot keep up with
// control traffic is already gone in every way that matters.
func (p *Peer) Send(msg *serverMessage) {
	select {
	case <-p.closed:
		return
	default:
	}
	select {
	case p.send <- msg:
	default:
		p.Close()
	}
}

// Close marks the peer disconnected. Safe to call repeatedly.
func (p *Peer) Close() {
	p.closeOne.Do(func() { close(p.closed) })
}

func (p *Peer) info(source string) peerInfo {
	return peerInfo{ID: p.ID, PubKey: p.PubKey, Source: source}
}

func (p *Peer) joinedRoom(r *Room) {
	p.mu.Lock()
	p.rooms[r.Key] = r
	p.mu.Unlock()
}

func (p *Peer) leftRoom(r *Room) {
	p.mu.Lock()
	delete(p.rooms, r.Key)
	p.mu.Unlock()
}

func (p *Peer) roomList() []*Room {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Room, 0, len(p.rooms))
	for _, r := range p.rooms {
		out = append(out, r)
	}
	return out
}

// CodeRoom returns the peer's code room, if it is in one.
func (p *Peer) CodeRoom() *Room {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.rooms {
		if r.Kind == roomKindCode {
			return r
		}
	}
	return nil
}

// Reachable reports every peer this one may address: everyone in any room
// it shares, itself excluded. A peer discovered on the local network and
// a peer who arrived by code are equally reachable; only the label
// differs, so the UI can explain where someone came from.
func (p *Peer) Reachable() []peerInfo {
	seen := make(map[string]peerInfo)
	for _, room := range p.roomList() {
		for _, other := range room.members() {
			if other.ID == p.ID {
				continue
			}
			// A code room is the stronger statement about intent, so it
			// wins the label when someone is visible through both.
			if existing, ok := seen[other.ID]; ok && existing.Source == roomKindCode {
				continue
			}
			seen[other.ID] = other.info(room.Kind)
		}
	}
	out := make([]peerInfo, 0, len(seen))
	for _, info := range seen {
		out = append(out, info)
	}
	return out
}

// Find resolves a peer this one is allowed to address.
func (p *Peer) Find(id string) (*Peer, bool) {
	for _, room := range p.roomList() {
		if other, ok := room.Peer(id); ok && other.ID != p.ID {
			return other, true
		}
	}
	return nil, false
}

// Room is a set of peers who can see each other.
type Room struct {
	// Key is the internal identifier. For code rooms it is the code; for
	// network rooms it is an opaque digest of the source network, so a
	// raw IP is never stored or exposed.
	Key  string
	Code string
	Kind string
	// NetworkKey binds both automatic and explicit rooms to one source
	// network. A room link can narrow who a user sees, but never bridge two
	// different networks.
	NetworkKey string

	mu         sync.RWMutex
	peers      map[string]*Peer
	lastActive time.Time
	maxPeers   int
}

func newRoom(key, code, kind string, maxPeers int) *Room {
	return &Room{
		Key:        key,
		Code:       code,
		Kind:       kind,
		peers:      make(map[string]*Peer),
		lastActive: time.Now(),
		maxPeers:   maxPeers,
	}
}

// Add places a peer in the room.
func (r *Room) Add(p *Peer) error {
	r.mu.Lock()
	if len(r.peers) >= r.maxPeers {
		r.mu.Unlock()
		if r.Kind == roomKindNetwork {
			return errNetworkBusy
		}
		return errRoomFull
	}
	r.peers[p.ID] = p
	r.lastActive = time.Now()
	r.mu.Unlock()
	p.joinedRoom(r)
	return nil
}

// Remove drops a peer and reports whether the room is now empty.
func (r *Room) Remove(p *Peer) (empty bool) {
	r.mu.Lock()
	delete(r.peers, p.ID)
	r.lastActive = time.Now()
	empty = len(r.peers) == 0
	r.mu.Unlock()
	p.leftRoom(r)
	return empty
}

// Peer looks up one member.
func (r *Room) Peer(id string) (*Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.peers[id]
	return p, ok
}

func (r *Room) members() []*Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	return out
}

// Broadcast delivers a frame to every member.
func (r *Room) Broadcast(msg *serverMessage) {
	for _, p := range r.members() {
		p.Send(msg)
	}
}

// NotifyRosters pushes each member its own view. Rosters are per-peer
// rather than per-room because two peers in the same room can see
// different people: one may also be in a code room the other is not.
func (r *Room) NotifyRosters() {
	for _, p := range r.members() {
		p.Send(rosterFor(p))
	}
}

func rosterFor(p *Peer) *serverMessage {
	msg := &serverMessage{Type: msgRoster, Peers: p.Reachable()}
	if code := p.CodeRoom(); code != nil {
		msg.Code = code.Code
	}
	return msg
}

// Touch marks the room as active, deferring its idle expiry.
func (r *Room) Touch() {
	r.mu.Lock()
	r.lastActive = time.Now()
	r.mu.Unlock()
}

func (r *Room) idleSince() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastActive
}

func (r *Room) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// Hub owns every room. State lives entirely in memory: a single coordinator
// instance has nothing worth persisting, and a restart losing all rooms is
// an acceptable, recoverable event that clients already handle.
type Hub struct {
	mu     sync.RWMutex
	rooms  map[string]*Room
	limits Limits
	logger *logger.Logger
	rl     *rateLimiter

	// networkSalt keeps address digests unlinkable across coordinator restarts
	// and useless to anyone who obtains one.
	networkSalt []byte
}

// NewHub constructs a hub. A nil logger is allowed; log calls become no-ops.
func NewHub(limits Limits, lg *logger.Logger) *Hub {
	limits = limits.withDefaults()
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		// Without a salt, address digests would be guessable. Fall back to
		// a time-derived value rather than an empty one.
		salt = []byte(time.Now().String())
	}
	return &Hub{
		rooms:       make(map[string]*Room),
		limits:      limits,
		logger:      lg,
		rl:          newRateLimiter(limits.CreatePerIPPerMin),
		networkSalt: salt,
	}
}

// networkGroup canonicalises the source used for candidate grouping. IPv4
// addresses are grouped exactly. IPv6 privacy addresses on the same LAN
// commonly differ below the routing prefix, so IPv6 is grouped by /64.
func networkGroup(ip string) string {
	trimmed := strings.TrimSpace(ip)
	parsed := net.ParseIP(trimmed)
	if parsed == nil {
		return trimmed
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// networkKey digests a canonical client network. The raw address never becomes a
// map key, so the room table cannot be read back as a list of who is
// online from where.
func (h *Hub) networkKey(ip string) string {
	sum := sha256.Sum256(append(h.networkSalt, []byte(networkGroup(ip))...))
	return "net:" + hex.EncodeToString(sum[:8])
}

// NetworkRoom returns the candidate group shared by everyone reaching the coordinator from
// the same public address — the browser's substitute for the CLI's UDP
// broadcast, which no browser can send.
//
// The grouping is only as good as the address. Devices on one Wi-Fi
// network share a public address, which is the case this serves. But so
// do devices behind carrier-grade NAT, where "same address" means
// "same ISP region" rather than "same room". MaxNetworkPeers bounds that:
// past the cap the coordinator stops grouping rather than introducing strangers
// to each other.
func (h *Hub) NetworkRoom(ip string) (*Room, error) {
	key := h.networkKey(ip)

	h.mu.Lock()
	defer h.mu.Unlock()
	if room, ok := h.rooms[key]; ok {
		return room, nil
	}
	if len(h.rooms) >= h.limits.MaxRooms {
		return nil, errAtCapacity
	}
	room := newRoom(key, "", roomKindNetwork, h.limits.MaxNetworkPeers)
	room.NetworkKey = key
	h.rooms[key] = room
	return room, nil
}

// CreateRoom allocates a code room. Codes are retried on the
// astronomically unlikely event of a collision rather than trusting
// randomness blindly.
func (h *Hub) CreateRoom(ip string) (*Room, error) {
	networkKey := h.networkKey(ip)
	if !h.rl.allow(networkGroup(ip), time.Now()) {
		return nil, errRateLimited
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) >= h.limits.MaxRooms {
		return nil, errAtCapacity
	}
	for attempt := 0; attempt < 8; attempt++ {
		code, err := newRoomCode()
		if err != nil {
			return nil, err
		}
		if _, taken := h.rooms[code]; taken {
			continue
		}
		room := newRoom(code, code, roomKindCode, h.limits.MaxPeersPerRoom)
		room.NetworkKey = networkKey
		h.rooms[code] = room
		return room, nil
	}
	return nil, errAtCapacity
}

// Room resolves a code for a caller on the room's source network, tolerating
// user formatting. Possessing a room link never grants cross-network access.
func (h *Hub) Room(code, ip string) (*Room, error) {
	normalized := normalizeCode(code)
	if normalized == "" {
		return nil, errRoomNotFound
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	room, ok := h.rooms[normalized]
	if !ok || room.Kind != roomKindCode {
		return nil, errRoomNotFound
	}
	if room.NetworkKey != h.networkKey(ip) {
		return nil, errNetworkMatch
	}
	return room, nil
}

// Drop removes a room, used when its last peer leaves.
func (h *Hub) Drop(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms, key)
}

// Rooms reports the current room count, for the health endpoint.
func (h *Hub) Rooms() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// Run sweeps idle rooms until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.sweep(now)
			h.rl.sweep(now)
		}
	}
}

func (h *Hub) sweep(now time.Time) {
	h.mu.Lock()
	stale := make([]*Room, 0)
	for key, room := range h.rooms {
		if room.size() > 0 || now.Sub(room.idleSince()) < h.limits.RoomIdleTTL {
			continue
		}
		stale = append(stale, room)
		delete(h.rooms, key)
	}
	h.mu.Unlock()

	for _, room := range stale {
		h.logf("coordinator: expired idle room %s", room.Key)
	}
}

func (h *Hub) logf(format string, args ...any) {
	if h.logger == nil {
		return
	}
	h.logger.Info(format, args...)
}
