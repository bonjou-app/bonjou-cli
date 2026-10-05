package relay

import (
	"errors"
	"runtime"
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
	if !h.Drop(room) {
		t.Fatal("Drop did not retire the empty room")
	}
	if _, err := h.Room(room.Code, "192.0.2.1"); !errors.Is(err, errRoomNotFound) {
		t.Fatalf("after Drop, lookup error = %v, want errRoomNotFound", err)
	}
	if h.Rooms() != 0 {
		t.Fatalf("Rooms() = %d, want 0", h.Rooms())
	}
}

func TestHubDropPreservesRoomRejoinedAfterLastRemoval(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	a, b, c := newPeer("a", ""), newPeer("b", ""), newPeer("c", "")
	room, err := h.joinNetwork("192.0.2.1", a)
	if err != nil {
		t.Fatalf("join a: %v", err)
	}
	// Reproduce a last-peer cleanup whose empty observation predates a join.
	if !room.Remove(a) {
		t.Fatal("room still has members after removing a")
	}
	joined, err := h.joinNetwork("192.0.2.1", b)
	if err != nil {
		t.Fatalf("join b: %v", err)
	}
	if joined != room {
		t.Fatal("b did not join the still-registered room")
	}
	if h.Drop(room) {
		t.Fatal("stale empty observation retired a room with a new member")
	}
	joined, err = h.joinNetwork("192.0.2.1", c)
	if err != nil {
		t.Fatalf("join c: %v", err)
	}
	if joined != room {
		t.Fatal("the rejoined room was orphaned from network discovery")
	}
	if _, ok := b.Find(c.ID); !ok {
		t.Fatal("new network members cannot address one another")
	}
}

func TestHubDropCannotRemoveReplacementNetworkRoom(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	a, b, c := newPeer("a", ""), newPeer("b", ""), newPeer("c", "")
	old, err := h.joinNetwork("192.0.2.1", a)
	if err != nil {
		t.Fatalf("join a: %v", err)
	}
	if !h.removePeer(old, a) {
		t.Fatal("last departure did not retire the old room")
	}
	replacement, err := h.joinNetwork("192.0.2.1", b)
	if err != nil {
		t.Fatalf("join b: %v", err)
	}
	if replacement == old || replacement.Key != old.Key {
		t.Fatal("network did not create a replacement under its existing key")
	}
	if h.Drop(old) {
		t.Fatal("stale room pointer retired its replacement")
	}
	// Repeated cleanup must also leave the replacement registered.
	h.removePeer(old, a)
	joined, err := h.joinNetwork("192.0.2.1", c)
	if err != nil {
		t.Fatalf("join c: %v", err)
	}
	if joined != replacement || h.Rooms() != 1 {
		t.Fatal("stale cleanup orphaned or removed the replacement room")
	}
	if _, ok := b.Find(c.ID); !ok {
		t.Fatal("replacement members cannot address one another")
	}
}

func TestHubCodeRoomDepartureAndJoinOrdering(t *testing.T) {
	for _, joinFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(joinFirst), func(t *testing.T) {
			h := NewHub(DefaultLimits(), nil)
			a, b := newPeer("a", ""), newPeer("b", "")
			room, err := h.createRoomForPeer("192.0.2.1", a)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if _, err := h.joinRoom(room.Code, "198.51.100.1", newPeer("outside", "")); !errors.Is(err, errNetworkMatch) {
				t.Fatalf("cross-network join = %v, want errNetworkMatch", err)
			}
			if joinFirst {
				joined, err := h.joinRoom(room.Code, "192.0.2.1", b)
				if err != nil || joined != room {
					t.Fatalf("join before departure = %v, room = %p", err, joined)
				}
				if h.removePeer(room, a) {
					t.Fatal("departure retired a code room with a joined member")
				}
				if found, err := h.Room(room.Code, "192.0.2.1"); err != nil || found != room {
					t.Fatalf("remaining member's room lookup = %v, room = %p", err, found)
				}
				h.sweep(time.Now().Add(2 * h.limits.RoomIdleTTL))
				if h.Rooms() != 1 || b.CodeRoom() != room {
					t.Fatal("idle retirement removed a populated code room")
				}
			} else {
				if !h.removePeer(room, a) {
					t.Fatal("last departure did not retire the code room")
				}
				if _, err := h.joinRoom(room.Code, "192.0.2.1", b); !errors.Is(err, errRoomNotFound) {
					t.Fatalf("join after retirement = %v, want errRoomNotFound", err)
				}
				if h.Rooms() != 0 || b.CodeRoom() != nil {
					t.Fatal("join attached a peer to a retired code room")
				}
			}
		})
	}
}

func TestHubJoinAndRetirementAreAtomic(t *testing.T) {
	for _, kind := range []string{roomKindNetwork, roomKindCode} {
		t.Run(kind, func(t *testing.T) {
			h := NewHub(DefaultLimits(), nil)
			a, b := newPeer("a", ""), newPeer("b", "")
			var room *Room
			var err error
			if kind == roomKindNetwork {
				room, err = h.joinNetwork("192.0.2.1", a)
			} else {
				room, err = h.createRoomForPeer("192.0.2.1", a)
			}
			if err != nil {
				t.Fatalf("initial join: %v", err)
			}
			// Pause membership insertion at the room lock. The joining operation
			// must retain the hub lock until membership exists, so final departure
			// cannot retire the pointer obtained by the joining connection.
			room.mu.Lock()
			locked := true
			defer func() {
				if locked {
					room.mu.Unlock()
				}
			}()
			joined := make(chan error, 1)
			go func() {
				if kind == roomKindNetwork {
					_, err := h.joinNetwork("192.0.2.1", b)
					joined <- err
				} else {
					_, err := h.joinRoom(room.Code, "192.0.2.1", b)
					joined <- err
				}
			}()
			waitForHubLock(t, h)
			removed := make(chan bool, 1)
			go func() { removed <- h.removePeer(room, a) }()
			room.mu.Unlock()
			locked = false
			select {
			case err := <-joined:
				if err != nil {
					t.Fatalf("join: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("join did not complete")
			}
			select {
			case empty := <-removed:
				if empty {
					t.Fatal("final departure retired a joining peer's room")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("departure did not complete")
			}
			if h.Rooms() != 1 {
				t.Fatal("joining peer's room is not registered")
			}
			if found, ok := room.Peer(b.ID); !ok || found != b {
				t.Fatal("joining peer is missing from the registered room")
			}
		})
	}
}

func waitForHubLock(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.mu.TryLock() {
		h.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("paused operation did not retain the hub lock")
		}
		runtime.Gosched()
	}
}

func TestHubRosterRefreshIsOrderedWithMembership(t *testing.T) {
	for _, refresh := range []string{"room", "hello"} {
		for _, change := range []string{"join", "departure"} {
			t.Run(refresh+"/"+change, func(t *testing.T) {
				h := NewHub(DefaultLimits(), nil)
				bob, alice := newPeer("bob", ""), newPeer("alice", "")
				room, err := h.joinNetwork("192.0.2.1", bob)
				if err != nil {
					t.Fatalf("join bob: %v", err)
				}
				if change == "departure" {
					if _, err := h.joinNetwork("192.0.2.1", alice); err != nil {
						t.Fatalf("join alice: %v", err)
					}
				}
				// Pause the actual roster snapshot at peer membership lookup.
				// Membership must stay blocked until that snapshot is enqueued.
				bob.mu.Lock()
				locked := true
				defer func() {
					if locked {
						bob.mu.Unlock()
					}
				}()
				notified := make(chan struct{})
				go func() {
					if refresh == "room" {
						h.notifyRosters(room)
					} else {
						h.sendRoster(bob)
					}
					close(notified)
				}()
				waitForHubLock(t, h)
				changed := make(chan error, 1)
				go func() {
					if change == "join" {
						_, err := h.joinNetwork("192.0.2.1", alice)
						changed <- err
					} else {
						h.removePeer(room, alice)
						changed <- nil
					}
				}()
				bob.mu.Unlock()
				locked = false
				select {
				case <-notified:
				case <-time.After(5 * time.Second):
					t.Fatal("roster notification did not finish")
				}
				select {
				case err := <-changed:
					if err != nil {
						t.Fatalf("membership change: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("membership change did not finish")
				}
				h.notifyRosters(room)
				if len(bob.send) != 2 {
					t.Fatalf("queued rosters = %d, want 2", len(bob.send))
				}
				first, last := <-bob.send, <-bob.send
				if first.Type != msgRoster || last.Type != msgRoster {
					t.Fatal("refresh did not queue roster messages")
				}
				before, after := 0, 1
				if change == "departure" {
					before, after = 1, 0
				}
				if len(first.Peers) != before || len(last.Peers) != after {
					t.Fatalf("roster order = %v then %v, want %d then %d peers", first.Peers, last.Peers, before, after)
				}
				if after == 1 && last.Peers[0].ID != alice.ID {
					t.Fatal("final roster lost the currently joined peer")
				}
			})
		}
	}
}

func TestHubRosterNotificationRejectsRetiredRoom(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	oldPeer, currentPeer := newPeer("old", ""), newPeer("current", "")
	old, err := h.joinNetwork("192.0.2.1", oldPeer)
	if err != nil {
		t.Fatalf("old join: %v", err)
	}
	h.removePeer(old, oldPeer)
	current, err := h.joinNetwork("192.0.2.1", currentPeer)
	if err != nil {
		t.Fatalf("current join: %v", err)
	}
	// An obsolete room reference must not publish rosters after retirement,
	// even if an old caller has retained and populated that reference.
	if err := old.Add(oldPeer); err != nil {
		t.Fatalf("populate obsolete reference: %v", err)
	}
	h.notifyRosters(old)
	if len(oldPeer.send) != 0 || len(currentPeer.send) != 0 {
		t.Fatal("retired room published a roster")
	}
	h.notifyRosters(current)
	if len(currentPeer.send) != 1 {
		t.Fatal("current room did not publish its roster")
	}
}

func TestHubDepartureNotifiesOnlyCurrentScope(t *testing.T) {
	for _, scope := range []string{"lobby", "shared-code", "moved-code"} {
		t.Run(scope, func(t *testing.T) {
			h := NewHub(DefaultLimits(), nil)
			alice, bob := newPeer("alice", ""), newPeer("bob", "")
			lobby, err := h.joinNetwork("192.0.2.1", alice)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.joinNetwork("192.0.2.1", bob); err != nil {
				t.Fatal(err)
			}
			code, err := h.createRoomForPeer("192.0.2.1", alice)
			if err != nil {
				t.Fatal(err)
			}
			if scope != "lobby" {
				if _, err := h.joinRoom(code.Code, "192.0.2.1", bob); err != nil {
					t.Fatal(err)
				}
			}
			if scope == "moved-code" {
				h.removePeer(lobby, bob)
			}
			h.departPeer(lobby, alice)
			if scope == "moved-code" {
				if len(bob.send) != 0 {
					t.Fatal("former lobby member received an obsolete departure")
				}
				h.notifyRosters(code)
			}
			if scope == "lobby" {
				if len(bob.send) != 2 {
					t.Fatalf("departure messages = %d, want 2", len(bob.send))
				}
				left, roster := <-bob.send, <-bob.send
				if left.Type != msgPeerLeft || left.PeerID != alice.ID || roster.Type != msgRoster || len(roster.Peers) != 0 {
					t.Fatal("departure was not followed by the current empty roster")
				}
			} else {
				if len(bob.send) != 1 {
					t.Fatalf("current code-room messages = %d, want 1", len(bob.send))
				}
				roster := <-bob.send
				if roster.Type != msgRoster || len(roster.Peers) != 1 || roster.Peers[0].ID != alice.ID || roster.Peers[0].Source != roomKindCode {
					t.Fatal("departure discarded a peer still reachable through the code room")
				}
			}
		})
	}
}

func TestHubDepartureCannotOvertakeScopeChange(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	alice, bob := newPeer("alice", ""), newPeer("bob", "")
	lobby, err := h.joinNetwork("192.0.2.1", alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.joinNetwork("192.0.2.1", bob); err != nil {
		t.Fatal(err)
	}
	code, err := h.createRoomForPeer("192.0.2.1", alice)
	if err != nil {
		t.Fatal(err)
	}
	// Pause recipient lookup inside the actual departure. Bob's scope change
	// must wait until the old departure and its correcting roster are queued.
	bob.mu.Lock()
	locked := true
	defer func() {
		if locked {
			bob.mu.Unlock()
		}
	}()
	departed := make(chan bool, 1)
	go func() { departed <- h.departPeer(lobby, alice) }()
	waitForHubLock(t, h)
	moved := make(chan error, 1)
	go func() {
		if _, err := h.joinRoom(code.Code, "192.0.2.1", bob); err != nil {
			moved <- err
			return
		}
		h.departPeer(lobby, bob)
		h.notifyRosters(code)
		moved <- nil
	}()
	bob.mu.Unlock()
	locked = false
	select {
	case empty := <-departed:
		if empty {
			t.Fatal("Alice's departure unexpectedly retired Bob's lobby")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("departure did not finish")
	}
	select {
	case err := <-moved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scope change did not finish")
	}
	if len(bob.send) != 3 {
		t.Fatalf("queued scope messages = %d, want 3", len(bob.send))
	}
	left, oldRoster, currentRoster := <-bob.send, <-bob.send, <-bob.send
	if left.Type != msgPeerLeft || left.PeerID != alice.ID || oldRoster.Type != msgRoster || len(oldRoster.Peers) != 0 {
		t.Fatal("old lobby departure did not precede its empty roster")
	}
	if currentRoster.Type != msgRoster || currentRoster.Code != code.Code || len(currentRoster.Peers) != 1 || currentRoster.Peers[0].ID != alice.ID {
		t.Fatal("old lobby departure overtook the current code-room roster")
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
