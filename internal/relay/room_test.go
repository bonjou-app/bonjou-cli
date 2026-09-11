package relay

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHubCreateAndLookupRoom(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	room, err := h.CreateRoom("192.0.2.1")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	// A user retyping the code casually must still land in the room.
	found, err := h.Room(strings.ToLower(room.Code), "192.0.2.1")
	if err != nil {
		t.Fatalf("Room: %v", err)
	}
	if found != room {
		t.Fatal("lookup returned a different room")
	}
	if _, err := h.Room("BBB-CCC", "192.0.2.1"); !errors.Is(err, errRoomNotFound) {
		t.Fatalf("unknown code error = %v, want errRoomNotFound", err)
	}
}

func TestHubDropRemovesRoom(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	room, err := h.CreateRoom("192.0.2.1")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	h.Drop(room.Code)
	if _, err := h.Room(room.Code, "192.0.2.1"); !errors.Is(err, errRoomNotFound) {
		t.Fatalf("after Drop, lookup error = %v, want errRoomNotFound", err)
	}
	if h.Rooms() != 0 {
		t.Fatalf("Rooms() = %d, want 0", h.Rooms())
	}
}

func TestRoomRejectsPeersBeyondLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPeersPerRoom = 2
	h := NewHub(limits, nil)
	room, err := h.CreateRoom("192.0.2.1")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := room.Add(newPeer(strconv.Itoa(i), "")); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := room.Add(newPeer("overflow", "")); !errors.Is(err, errRoomFull) {
		t.Fatalf("third Add error = %v, want errRoomFull", err)
	}
}

func TestRoomRemoveReportsEmpty(t *testing.T) {
	room := newRoom("7K2-9QX", "7K2-9QX", roomKindCode, 8)
	a, b := newPeer("a", ""), newPeer("b", "")
	if err := room.Add(a); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if err := room.Add(b); err != nil {
		t.Fatalf("Add b: %v", err)
	}
	if empty := room.Remove(a); empty {
		t.Fatal("room reported empty while one peer remains")
	}
	if empty := room.Remove(b); !empty {
		t.Fatal("room did not report empty after last peer left")
	}
}

// A peer sees everyone else in the room, never itself.
func TestReachableExcludesSelf(t *testing.T) {
	room := newRoom("7K2-9QX", "7K2-9QX", roomKindCode, 8)
	ada, bo := newPeer("a", "aa"), newPeer("b", "bb")
	for _, p := range []*Peer{ada, bo} {
		if err := room.Add(p); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	reachable := ada.Reachable()
	if len(reachable) != 1 {
		t.Fatalf("Ada sees %d peers, want 1", len(reachable))
	}
	if reachable[0].ID != "b" || reachable[0].PubKey != "bb" {
		t.Fatalf("Ada sees %+v, want peer b", reachable[0])
	}
	if reachable[0].Source != roomKindCode {
		t.Fatalf("source = %q, want %q", reachable[0].Source, roomKindCode)
	}
}

// The union is the point: someone on your Wi-Fi and someone who used your
// link are both reachable, and each is labelled with where they came from.
func TestReachableUnionsNetworkAndCodeRooms(t *testing.T) {
	net := newRoom("net:abc", "", roomKindNetwork, 12)
	code := newRoom("7K2-9QX", "7K2-9QX", roomKindCode, 8)

	me := newPeer("me", "00")
	neighbour := newPeer("n", "11")
	invitee := newPeer("i", "22")

	for _, p := range []*Peer{me, neighbour} {
		if err := net.Add(p); err != nil {
			t.Fatalf("net Add: %v", err)
		}
	}
	for _, p := range []*Peer{me, invitee} {
		if err := code.Add(p); err != nil {
			t.Fatalf("code Add: %v", err)
		}
	}

	sources := map[string]string{}
	for _, info := range me.Reachable() {
		sources[info.ID] = info.Source
	}
	if len(sources) != 2 {
		t.Fatalf("reachable = %v, want two peers", sources)
	}
	if sources["n"] != roomKindNetwork {
		t.Errorf("neighbour source = %q, want %q", sources["n"], roomKindNetwork)
	}
	if sources["i"] != roomKindCode {
		t.Errorf("invitee source = %q, want %q", sources["i"], roomKindCode)
	}

	// Reachability is what authorises addressing someone at all.
	if _, ok := me.Find("n"); !ok {
		t.Error("neighbour not addressable")
	}
	if _, ok := me.Find("me"); ok {
		t.Error("a peer must not be able to address itself")
	}
	if _, ok := neighbour.Find("i"); ok {
		t.Error("a network peer must not reach into an unrelated code room")
	}
}

// Someone visible through both rooms is listed once, labelled by the
// stronger signal of intent.
func TestReachableDeduplicatesAcrossRooms(t *testing.T) {
	net := newRoom("net:abc", "", roomKindNetwork, 12)
	code := newRoom("7K2-9QX", "7K2-9QX", roomKindCode, 8)
	me, both := newPeer("me", "00"), newPeer("b", "11")
	for _, room := range []*Room{net, code} {
		for _, p := range []*Peer{me, both} {
			if err := room.Add(p); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
	}
	reachable := me.Reachable()
	if len(reachable) != 1 {
		t.Fatalf("reachable = %+v, want one entry", reachable)
	}
	if reachable[0].Source != roomKindCode {
		t.Errorf("source = %q, want %q (code is the stronger signal)", reachable[0].Source, roomKindCode)
	}
}

// Carrier-grade NAT can put hundreds of unrelated people behind one
// address. Past the cap the coordinator must stop grouping rather than
// introduce strangers to each other.
func TestNetworkRoomStopsGroupingPastCap(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxNetworkPeers = 2
	h := NewHub(limits, nil)

	room, err := h.NetworkRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := room.Add(newPeer(strconv.Itoa(i), "")); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := room.Add(newPeer("overflow", "")); !errors.Is(err, errNetworkBusy) {
		t.Fatalf("Add past cap = %v, want errNetworkBusy", err)
	}
}

// Same address means same room; a different address must not.
func TestNetworkRoomGroupsByAddress(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	a, err := h.NetworkRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	again, err := h.NetworkRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	if a != again {
		t.Fatal("the same address produced two different rooms")
	}
	other, err := h.NetworkRoom("198.51.100.4")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	if a == other {
		t.Fatal("different addresses were grouped together")
	}
}

func TestNetworkRoomGroupsIPv6ByPrefix(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	a, err := h.NetworkRoom("2001:db8:abcd:12::1")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	samePrefix, err := h.NetworkRoom("2001:db8:abcd:12:ffff::9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	if a != samePrefix {
		t.Fatal("IPv6 addresses in the same /64 produced different rooms")
	}
	otherPrefix, err := h.NetworkRoom("2001:db8:abcd:13::1")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	if a == otherPrefix {
		t.Fatal("IPv6 addresses in different /64 prefixes were grouped")
	}
}

func TestCodeRoomCannotBridgeNetworks(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	room, err := h.CreateRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := h.Room(room.Code, "203.0.113.9"); err != nil {
		t.Fatalf("same-network lookup: %v", err)
	}
	if _, err := h.Room(room.Code, "198.51.100.4"); !errors.Is(err, errNetworkMatch) {
		t.Fatalf("different-network lookup error = %v, want errNetworkMatch", err)
	}
}

func TestDefaultLimitsSupportOneHundredLANPeers(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxNetworkPeers < 100 {
		t.Fatalf("MaxNetworkPeers = %d, want at least 100", limits.MaxNetworkPeers)
	}
	h := NewHub(limits, nil)
	room, err := h.NetworkRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := room.Add(newPeer(strconv.Itoa(i), "key")); err != nil {
			t.Fatalf("Add peer %d: %v", i, err)
		}
	}
	if got := room.size(); got != 100 {
		t.Fatalf("room size = %d, want 100", got)
	}
}

// The room table must not double as a list of who is online from where.
func TestNetworkKeyDoesNotLeakAddress(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	key := h.networkKey("203.0.113.9")
	if strings.Contains(key, "203.0.113.9") {
		t.Fatalf("network key %q contains the raw address", key)
	}
	// A second hub uses a fresh salt, so digests are not comparable
	// across restarts either.
	if other := NewHub(DefaultLimits(), nil).networkKey("203.0.113.9"); other == key {
		t.Fatal("network keys are stable across hubs; the salt is not doing its job")
	}
}

// A code room must not be reachable through the network-room lookup, or a
// guessed digest would open someone else's private room.
func TestRoomLookupRejectsNetworkRooms(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	room, err := h.NetworkRoom("203.0.113.9")
	if err != nil {
		t.Fatalf("NetworkRoom: %v", err)
	}
	if _, err := h.Room(room.Key, "203.0.113.9"); !errors.Is(err, errRoomNotFound) {
		t.Fatalf("network room resolved by code lookup: %v", err)
	}
}

func TestHubSweepExpiresIdleRooms(t *testing.T) {
	limits := DefaultLimits()
	limits.RoomIdleTTL = time.Minute
	h := NewHub(limits, nil)
	room, err := h.CreateRoom("192.0.2.1")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	peer := newPeer("a", "")
	if err := room.Add(peer); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// An occupied room is never swept, however quiet it has been. Someone
	// waiting in a room for a slow friend must not have it vanish.
	h.sweep(time.Now().Add(2 * time.Minute))
	if h.Rooms() != 1 {
		t.Fatalf("occupied room was swept; Rooms() = %d, want 1", h.Rooms())
	}

	room.Remove(peer)
	h.sweep(time.Now())
	if h.Rooms() != 1 {
		t.Fatalf("room expired early; Rooms() = %d, want 1", h.Rooms())
	}
	h.sweep(time.Now().Add(2 * time.Minute))
	if h.Rooms() != 0 {
		t.Fatalf("idle empty room survived sweep; Rooms() = %d, want 0", h.Rooms())
	}
}

func TestRoomTouchDefersExpiry(t *testing.T) {
	limits := DefaultLimits()
	limits.RoomIdleTTL = time.Minute
	h := NewHub(limits, nil)
	room, err := h.CreateRoom("192.0.2.1")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	later := time.Now().Add(2 * time.Minute)
	room.mu.Lock()
	room.lastActive = later
	room.mu.Unlock()

	h.sweep(later)
	if h.Rooms() != 1 {
		t.Fatalf("recently active room was swept; Rooms() = %d, want 1", h.Rooms())
	}
}

// A client that stops reading its socket must not be able to make the
// coordinator buffer for it indefinitely.
func TestPeerSendClosesSlowClient(t *testing.T) {
	p := newPeer("a", "")
	for i := 0; i < peerSendBuffer; i++ {
		p.Send(&serverMessage{Type: msgRoster})
	}
	select {
	case <-p.closed:
		t.Fatal("peer closed before its buffer was full")
	default:
	}
	p.Send(&serverMessage{Type: msgRoster})
	select {
	case <-p.closed:
	default:
		t.Fatal("peer was not closed after overflowing its send buffer")
	}
}

func TestPeerCloseIsIdempotent(t *testing.T) {
	p := newPeer("a", "")
	p.Close()
	p.Close()
	select {
	case <-p.closed:
	default:
		t.Fatal("peer not closed")
	}
}

func TestRateLimiterWindow(t *testing.T) {
	rl := newRateLimiter(3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !rl.allow("192.0.2.1", now) {
			t.Fatalf("request %d denied inside the limit", i)
		}
	}
	if rl.allow("192.0.2.1", now) {
		t.Fatal("fourth request allowed past the limit")
	}
	// A different source has its own budget.
	if !rl.allow("192.0.2.2", now) {
		t.Fatal("unrelated address was rate limited")
	}
	// The window rolls over.
	if !rl.allow("192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("request denied after the window rolled over")
	}
}

func TestRateLimiterSweepDropsStaleWindows(t *testing.T) {
	rl := newRateLimiter(3)
	now := time.Now()
	rl.allow("192.0.2.1", now)
	rl.sweep(now.Add(3 * time.Minute))
	rl.mu.Lock()
	remaining := len(rl.windows)
	rl.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("stale windows remaining = %d, want 0", remaining)
	}
}

func TestHubCreateRoomRateLimited(t *testing.T) {
	limits := DefaultLimits()
	limits.CreatePerIPPerMin = 2
	h := NewHub(limits, nil)
	for i := 0; i < 2; i++ {
		if _, err := h.CreateRoom("192.0.2.1"); err != nil {
			t.Fatalf("CreateRoom %d: %v", i, err)
		}
	}
	if _, err := h.CreateRoom("192.0.2.1"); !errors.Is(err, errRateLimited) {
		t.Fatalf("third CreateRoom error = %v, want errRateLimited", err)
	}
}

func TestHubCapacity(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxRooms = 1
	h := NewHub(limits, nil)
	if _, err := h.CreateRoom("192.0.2.1"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := h.CreateRoom("192.0.2.2"); !errors.Is(err, errAtCapacity) {
		t.Fatalf("second CreateRoom error = %v, want errAtCapacity", err)
	}
}

func TestCodeForError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{errRoomNotFound, errCodeNoRoom},
		{errRoomFull, errCodeRoomFull},
		{errNetworkBusy, errCodeNetworkBusy},
		{errNetworkMatch, errCodeNetworkMatch},
		{errUnsupported, errCodeUnsupported},
		{errPeerNotFound, errCodeNoPeer},
		{errRateLimited, errCodeRateLimited},
		{errAtCapacity, errCodeCapacity},
		{errAlreadyInRoom, errCodeAlreadyInRoom},
		{errNotInRoom, errCodeNotInRoom},
		{errors.New("something else"), errCodeBadRequest},
	}
	for _, tc := range tests {
		if got := codeForError(tc.err); got != tc.want {
			t.Errorf("codeForError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
