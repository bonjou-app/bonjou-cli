package relay

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// wsReadLimit bounds one coordinator frame. It carries only a small
	// encrypted WebRTC description or ICE candidate, never app data.
	wsReadLimit = 128 << 10

	maxSignalPayloadLen = 96 << 10

	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 30 * time.Second

	pubKeyHexLen = 64 // X25519 public key: 32 bytes
)

// Conn is one browser's control-plane connection: a WebSocket carrying
// room membership and opaque end-to-end encrypted frames.
type Conn struct {
	ws   *websocket.Conn
	hub  *Hub
	ip   string
	peer *Peer

	netRoom  *Room
	codeRoom *Room
}

func newConn(ws *websocket.Conn, hub *Hub, ip string) (*Conn, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	return &Conn{ws: ws, hub: hub, ip: ip, peer: newPeer(id, "")}, nil
}

// run drives the read loop until the client disconnects or misbehaves.
func (c *Conn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.cleanup()

	c.ws.SetReadLimit(wsReadLimit)
	go c.writePump(ctx)

	for {
		var msg clientMessage
		if err := wsjson.Read(ctx, c.ws, &msg); err != nil {
			return
		}
		if err := c.handle(&msg); err != nil {
			c.peer.Send(errorMessage(codeForError(err), err.Error()))
			// Protocol misuse is not fatal on its own: a client that asks
			// for a room that expired should be told so and allowed to
			// create a new one rather than dropped.
			continue
		}
	}
}

// writePump owns all writes to the socket. Concentrating them here means
// no two goroutines can interleave frames, and keepalive pings share the
// same serialisation.
func (c *Conn) writePump(ctx context.Context) {
	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.peer.closed:
			_ = c.ws.Close(websocket.StatusPolicyViolation, "client not reading")
			return
		case msg := <-c.peer.send:
			wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := wsjson.Write(wctx, c.ws, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (c *Conn) handle(msg *clientMessage) error {
	switch msg.Type {
	case msgHello:
		return c.handleHello(msg)
	case msgCreate:
		return c.handleCreate(msg)
	case msgJoin:
		return c.handleJoin(msg)
	case msgSignal:
		return c.handleSignal(msg)
	case "relay", "transfer_begin", "transfer_end":
		return errUnsupported
	default:
		return fmt.Errorf("unknown message type %q", msg.Type)
	}
}

// handleHello is the entry point every client uses. It establishes the
// peer's session key and places it in the candidate group shared by everyone
// reaching the coordinator from the same source address. It is the browser's stand-in for the
// LAN broadcast the CLI uses, which no browser can send.
//
// Being unable to group by network is not an error the user can act on:
// they still get a working page, just without automatic neighbours, so it
// resolves to an empty roster with a reason rather than a failure.
func (c *Conn) handleHello(msg *clientMessage) error {
	if c.peer.PubKey == "" {
		if err := c.adoptKey(msg.PubKey); err != nil {
			return err
		}
	} else if msg.PubKey != "" && msg.PubKey != c.peer.PubKey {
		return errors.New("public key cannot change during a session")
	}
	if c.netRoom != nil || c.codeRoom != nil {
		c.hub.sendRoster(c.peer)
		return nil
	}

	room, err := c.hub.joinNetwork(c.ip, c.peer)
	if err != nil {
		c.peer.Send(&serverMessage{Type: msgJoined, PeerID: c.peer.ID})
		c.peer.Send(errorMessage(codeForError(err), err.Error()))
		return nil
	}
	c.netRoom = room
	c.peer.Send(&serverMessage{Type: msgJoined, PeerID: c.peer.ID})
	c.hub.notifyRosters(room)
	return nil
}

func (c *Conn) handleCreate(msg *clientMessage) error {
	if c.codeRoom != nil {
		return errAlreadyInRoom
	}
	if c.peer.PubKey == "" {
		return errNotInRoom
	}
	room, err := c.hub.createRoomForPeer(c.ip, c.peer)
	if err != nil {
		return err
	}
	c.enterCodeRoom(room)
	c.peer.Send(&serverMessage{Type: msgCreated, Code: room.Code, PeerID: c.peer.ID})
	c.notifyEveryone()
	c.hub.logf("coordinator: room %s created by %s", room.Code, c.peer.ID)
	return nil
}

func (c *Conn) handleJoin(msg *clientMessage) error {
	if c.codeRoom != nil {
		return errAlreadyInRoom
	}
	if c.peer.PubKey == "" {
		return errNotInRoom
	}
	room, err := c.hub.joinRoom(msg.Code, c.ip, c.peer)
	if err != nil {
		return err
	}
	c.enterCodeRoom(room)
	c.peer.Send(&serverMessage{Type: msgJoined, Code: room.Code, PeerID: c.peer.ID})
	c.notifyEveryone()
	return nil
}

// enterCodeRoom moves a peer out of the open network lobby. A room is a
// narrowing boundary: lobby users cannot see or address room members, while
// room members see only one another.
func (c *Conn) enterCodeRoom(room *Room) {
	c.codeRoom = room
	previous := c.netRoom
	c.netRoom = nil
	if previous == nil {
		return
	}
	c.hub.departPeer(previous, c.peer)
}

// handleSignal forwards one encrypted WebRTC negotiation frame. The
// coordinator reads the destination and nothing else. App messages and file
// bytes are deliberately not accepted on this socket.
func (c *Conn) handleSignal(msg *clientMessage) error {
	if c.peer.PubKey == "" {
		return errNotInRoom
	}
	if msg.Payload == "" {
		return errors.New("signal frame has empty payload")
	}
	if len(msg.Payload) > maxSignalPayloadLen {
		return errors.New("signal payload is too large")
	}
	target, ok := c.peer.Find(msg.To)
	if !ok {
		return errPeerNotFound
	}
	target.Send(&serverMessage{Type: msgSignal, From: c.peer.ID, Payload: msg.Payload})
	c.touchRooms()
	return nil
}

// adoptKey validates and records the ephemeral public key used by browsers to
// encrypt their WebRTC signaling. Display names are exchanged only after the
// direct data channel opens, so the coordinator never receives them.
func (c *Conn) adoptKey(pubKey string) error {
	if len(pubKey) != pubKeyHexLen {
		return fmt.Errorf("public key must be %d hex characters", pubKeyHexLen)
	}
	if _, err := hex.DecodeString(pubKey); err != nil {
		return errors.New("public key is not valid hex")
	}
	c.peer.PubKey = pubKey
	return nil
}

// notifyEveryone refreshes the roster for every peer who can see this one.
func (c *Conn) notifyEveryone() {
	if c.netRoom != nil {
		c.hub.notifyRosters(c.netRoom)
	}
	if c.codeRoom != nil {
		c.hub.notifyRosters(c.codeRoom)
	}
}

func (c *Conn) touchRooms() {
	if c.netRoom != nil {
		c.netRoom.Touch()
	}
	if c.codeRoom != nil {
		c.codeRoom.Touch()
	}
}

func (c *Conn) cleanup() {
	c.peer.Close()

	for _, room := range []*Room{c.netRoom, c.codeRoom} {
		if room == nil {
			continue
		}
		empty := c.hub.departPeer(room, c.peer)
		if empty {
			if room.Kind == roomKindCode {
				c.hub.logf("coordinator: room %s closed (last peer left)", room.Code)
			}
		}
	}
}

func codeForError(err error) string {
	switch {
	case errors.Is(err, errRoomNotFound):
		return errCodeNoRoom
	case errors.Is(err, errRoomFull):
		return errCodeRoomFull
	case errors.Is(err, errNetworkBusy):
		return errCodeNetworkBusy
	case errors.Is(err, errNetworkMatch):
		return errCodeNetworkMatch
	case errors.Is(err, errUnsupported):
		return errCodeUnsupported
	case errors.Is(err, errPeerNotFound):
		return errCodeNoPeer
	case errors.Is(err, errRateLimited):
		return errCodeRateLimited
	case errors.Is(err, errAtCapacity):
		return errCodeCapacity
	case errors.Is(err, errAlreadyInRoom):
		return errCodeAlreadyInRoom
	case errors.Is(err, errNotInRoom):
		return errCodeNotInRoom
	default:
		return errCodeBadRequest
	}
}
