package relay

import (
	"errors"
	"strings"
	"testing"
)

func testConn(t *testing.T, h *Hub, ip, key string) *Conn {
	t.Helper()
	c, err := newConn(nil, h, ip)
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	if err := c.handle(&clientMessage{Type: msgHello, PubKey: key}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	drainPeer(c.peer)
	return c
}

func drainPeer(p *Peer) {
	for {
		select {
		case <-p.send:
		default:
			return
		}
	}
}

func TestHelloDoesNotAcceptOrExposeAName(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	c := testConn(t, h, "203.0.113.9", strings.Repeat("a", pubKeyHexLen))
	info := c.peer.info(roomKindNetwork)
	if info.ID != c.peer.ID || info.PubKey != strings.Repeat("a", pubKeyHexLen) {
		t.Fatalf("peer info = %+v", info)
	}
}

func TestSignalForwardsOnlyWithinReachableNetwork(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	a := testConn(t, h, "203.0.113.9", strings.Repeat("a", pubKeyHexLen))
	b := testConn(t, h, "203.0.113.9", strings.Repeat("b", pubKeyHexLen))
	drainPeer(a.peer)
	drainPeer(b.peer)

	const payload = "opaque-encrypted-offer"
	if err := a.handle(&clientMessage{Type: msgSignal, To: b.peer.ID, Payload: payload}); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case got := <-b.peer.send:
		if got.Type != msgSignal || got.From != a.peer.ID || got.Payload != payload {
			t.Fatalf("forwarded signal = %+v", got)
		}
	default:
		t.Fatal("target did not receive signal")
	}

	outside := testConn(t, h, "198.51.100.4", strings.Repeat("c", pubKeyHexLen))
	if err := a.handle(&clientMessage{Type: msgSignal, To: outside.peer.ID, Payload: payload}); !errors.Is(err, errPeerNotFound) {
		t.Fatalf("cross-network signal error = %v, want errPeerNotFound", err)
	}
}

func TestPayloadRelayMessagesAreRejected(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	c := testConn(t, h, "203.0.113.9", strings.Repeat("a", pubKeyHexLen))
	for _, kind := range []string{"relay", "transfer_begin", "transfer_end"} {
		if err := c.handle(&clientMessage{Type: kind}); !errors.Is(err, errUnsupported) {
			t.Errorf("%s error = %v, want errUnsupported", kind, err)
		}
	}
}

func TestRoomJoinIsLimitedToCreatorsNetwork(t *testing.T) {
	h := NewHub(DefaultLimits(), nil)
	creator := testConn(t, h, "203.0.113.9", strings.Repeat("a", pubKeyHexLen))
	if err := creator.handle(&clientMessage{Type: msgCreate}); err != nil {
		t.Fatalf("create: %v", err)
	}
	code := creator.codeRoom.Code
	if creator.netRoom != nil {
		t.Fatal("room creator remained in the open network lobby")
	}

	local := testConn(t, h, "203.0.113.9", strings.Repeat("b", pubKeyHexLen))
	if _, ok := local.peer.Find(creator.peer.ID); ok {
		t.Fatal("open-lobby peer can address a room member")
	}
	if err := local.handle(&clientMessage{Type: msgJoin, Code: code}); err != nil {
		t.Fatalf("same-network join: %v", err)
	}
	if local.netRoom != nil {
		t.Fatal("room joiner remained in the open network lobby")
	}
	if _, ok := local.peer.Find(creator.peer.ID); !ok {
		t.Fatal("members of the same room cannot address each other")
	}

	remote := testConn(t, h, "198.51.100.4", strings.Repeat("c", pubKeyHexLen))
	if err := remote.handle(&clientMessage{Type: msgJoin, Code: code}); !errors.Is(err, errNetworkMatch) {
		t.Fatalf("different-network join error = %v, want errNetworkMatch", err)
	}
}
