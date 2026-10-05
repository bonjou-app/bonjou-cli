// Package relay implements the Bonjou web coordinator: a stateless service
// that groups browser candidates by source network and forwards opaque WebRTC
// signaling between candidates that are allowed to meet.
//
// The coordinator never carries profile data, chat messages, file metadata,
// or file bytes. Those travel only over direct browser-to-browser data
// channels. Nothing in this package imports internal/network: the coordinator
// has no key material and no way to read the signaling it forwards.
package relay

// Control-plane message kinds. The coordinator reads only the outer routing
// fields of each frame; anything in Payload is ciphertext it cannot open.
const (
	// Client to server.
	msgHello  = "hello"
	msgCreate = "create"
	msgJoin   = "join"
	msgSignal = "signal"

	// Server to client.
	msgCreated  = "created"
	msgJoined   = "joined"
	msgRoster   = "roster"
	msgPeerLeft = "peer_left"
	msgError    = "error"
)

// Error codes sent to clients. These are stable identifiers the frontend
// switches on; the accompanying message is for humans only.
const (
	errCodeBadRequest    = "bad_request"
	errCodeNoRoom        = "no_room"
	errCodeRoomFull      = "room_full"
	errCodeNoPeer        = "no_peer"
	errCodeRateLimited   = "rate_limited"
	errCodeCapacity      = "capacity"
	errCodeAlreadyInRoom = "already_in_room"
	errCodeNotInRoom     = "not_in_room"
	errCodeNetworkBusy   = "network_busy"
	errCodeNetworkMatch  = "network_mismatch"
	errCodeUnsupported   = "unsupported_message"
)

// clientMessage is an inbound control frame. Fields are optional per kind;
// unpopulated fields are ignored rather than rejected so the protocol can
// gain fields without breaking older clients.
type clientMessage struct {
	Type string `json:"type"`

	// hello
	PubKey string `json:"pubkey,omitempty"`

	// join
	Code string `json:"code,omitempty"`

	// signal
	To string `json:"to,omitempty"`

	// signal is an encrypted WebRTC offer, answer, or ICE candidate. The
	// coordinator forwards it verbatim and cannot inspect it.
	Payload string `json:"payload,omitempty"`
}

// serverMessage is an outbound control frame.
type serverMessage struct {
	Type string `json:"type"`

	Code   string     `json:"code,omitempty"`
	PeerID string     `json:"peer_id,omitempty"`
	Peers  []peerInfo `json:"peers,omitempty"`

	From    string `json:"from,omitempty"`
	Payload string `json:"payload,omitempty"`

	ErrCode string `json:"code_error,omitempty"`
	Message string `json:"message,omitempty"`
}

// peerInfo is one entry in a room roster. PubKey is the peer's ephemeral
// X25519 public key, hex-encoded; the coordinator forwards it verbatim and never
// uses it.
type peerInfo struct {
	ID     string `json:"id"`
	PubKey string `json:"pubkey"`
	// Source is "network" for peers found on the same public address and
	// "code" for peers who entered a shared code, so the UI can say where
	// somebody came from instead of presenting strangers and invitees
	// identically.
	Source string `json:"source"`
}

func errorMessage(code, message string) *serverMessage {
	return &serverMessage{Type: msgError, ErrCode: code, Message: message}
}
