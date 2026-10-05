/**
 * Platform-independent implementation of internal/relay's control protocol.
 * The caller validates source networks and owns WebSocket framing/transport.
 * Returned deliveries are transient: signaling is never retained in this hub.
 */

const CODE_ALPHABET = "23456789BCDFGHJKMNPQRSTVWXZ";
const MAX_ROOMS = 5000;
const MAX_PEERS = 128;
const CREATE_PER_MINUTE = 20;
const MAX_SIGNAL_BYTES = 96 << 10;
const MINUTE = 60_000;
const encoder = new TextEncoder();

const errors = Object.freeze({
  no_room: "no room with that code — it may have expired",
  room_full: "room is full",
  capacity: "coordinator is at capacity, try again shortly",
  no_peer: "that peer is no longer reachable",
  rate_limited: "too many rooms created from this address",
  already_in_room: "this connection is already in a room",
  not_in_room: "say hello first",
  network_busy: "too many devices share this network address to group them safely",
  network_mismatch: "that room belongs to a different network",
  unsupported_message: "message type is no longer supported",
});

function errorDelivery(id, code, message = errors[code]) {
  return { id, frame: { type: "error", code_error: code, message } };
}

function validKey(value) {
  return typeof value === "string" && /^[a-fA-F0-9]{64}$/.test(value);
}

function normalizeCode(raw) {
  // Go uses Unicode's simple uppercase mapping, which does not expand a
  // character such as ß into two symbols. Process each rune accordingly.
  let bare = "";
  for (const rune of raw) {
    const upper = rune.toUpperCase();
    if (upper.length === 1 && CODE_ALPHABET.includes(upper)) bare += upper;
  }
  return bare.length === 6 ? `${bare.slice(0, 3)}-${bare.slice(3)}` : "";
}

function clientMessage(frame) {
  // encoding/json accepts null as a zero-valued struct, ignores unknown
  // fields, matches field names case-insensitively, and rejects non-string
  // values in its known string fields. Such decode errors are socket-fatal.
  if (frame === null) frame = {};
  if (typeof frame !== "object" || Array.isArray(frame)) {
    throw new TypeError("control frame must be a JSON object");
  }
  const result = { type: "", pubkey: "", code: "", to: "", payload: "" };
  for (const [field, value] of Object.entries(frame)) {
    const key = field.toLowerCase();
    if (!Object.hasOwn(result, key) || value === null) continue;
    if (typeof value !== "string") {
      throw new TypeError(`control field ${key} must be a string`);
    }
    result[key] = value;
  }
  return result;
}

function defaultRandomBytes(length) {
  return globalThis.crypto.getRandomValues(new Uint8Array(length));
}

export class Coordinator {
  #peers = new Map();
  #rooms = new Map();
  #rates = new Map();
  #now;
  #randomBytes;

  constructor({ now = Date.now, randomBytes = defaultRandomBytes } = {}) {
    this.#now = now;
    this.#randomBytes = randomBytes;
  }

  get roomCount() {
    return this.#rooms.size;
  }

  addPeer({ id, network }) {
    if (typeof id !== "string" || !id || typeof network !== "string" || !network) {
      throw new TypeError("peer id and network are required");
    }
    if (this.#peers.has(id)) throw new TypeError("duplicate peer id");
    this.#peers.set(id, { id, network, pubkey: "", code: "", lobby: false });
    return [];
  }

  restorePeer(attachment) {
    if (
      !attachment || attachment.version !== 1 ||
      typeof attachment.id !== "string" || !attachment.id ||
      typeof attachment.network !== "string" || !attachment.network ||
      typeof attachment.pubkey !== "string" ||
      (attachment.pubkey !== "" && !validKey(attachment.pubkey)) ||
      typeof attachment.code !== "string" ||
      (attachment.code !== "" && normalizeCode(attachment.code) !== attachment.code) ||
      typeof attachment.lobby !== "boolean" ||
      (attachment.lobby && attachment.code !== "") ||
      (attachment.pubkey === "" && (attachment.lobby || attachment.code !== "")) ||
      this.#peers.has(attachment.id)
    ) {
      throw new TypeError("invalid peer attachment");
    }
    const peer = {
      id: attachment.id,
      network: attachment.network,
      pubkey: attachment.pubkey,
      code: attachment.code,
      lobby: attachment.lobby,
    };
    if (peer.lobby || peer.code) {
      const key = this.#roomKey(peer);
      let room = this.#rooms.get(key);
      if (
        (room && (room.network !== peer.network || room.members.size >= MAX_PEERS)) ||
        (!room && this.#rooms.size >= MAX_ROOMS)
      ) {
        throw new TypeError("peer attachment exceeds room boundaries");
      }
      if (!room) {
        room = this.#newRoom(peer.network, peer.code);
        this.#rooms.set(key, room);
      }
      room.members.add(peer.id);
    }
    this.#peers.set(peer.id, peer);
    return [];
  }

  exportPeer(id) {
    const peer = this.#peers.get(id);
    return peer ? { version: 1, ...peer } : undefined;
  }

  exportRates() {
    this.#sweepRates(this.#now());
    return [...this.#rates].map(([network, window]) => ({ network, ...window }));
  }

  restoreRates(rates) {
    if (!Array.isArray(rates)) throw new TypeError("invalid creation-rate windows");
    const restored = new Map();
    for (const window of rates) {
      if (
        !window || typeof window.network !== "string" || !window.network ||
        !Number.isSafeInteger(window.start) || window.start < 0 ||
        !Number.isInteger(window.count) || window.count < 1 ||
        window.count > CREATE_PER_MINUTE || restored.has(window.network)
      ) {
        throw new TypeError("invalid creation-rate window");
      }
      restored.set(window.network, { start: window.start, count: window.count });
    }
    this.#rates = restored;
    this.#sweepRates(this.#now());
    return [];
  }

  message(id, frame) {
    const peer = this.#peers.get(id);
    if (!peer) return [];
    const msg = clientMessage(frame);
    switch (msg.type) {
      case "hello": return this.#hello(peer, msg.pubkey);
      case "create": return this.#create(peer);
      case "join": return this.#join(peer, msg.code);
      case "signal": return this.#signal(peer, msg.to, msg.payload);
      case "relay":
      case "transfer_begin":
      case "transfer_end":
        return [errorDelivery(id, "unsupported_message")];
      default:
        return [errorDelivery(id, "bad_request", `unknown message type ${JSON.stringify(msg.type)}`)];
    }
  }

  removePeer(id) {
    const peer = this.#peers.get(id);
    if (!peer) return [];
    this.#peers.delete(id);
    return this.#depart(peer);
  }

  #hello(peer, pubkey) {
    if (peer.pubkey === "") {
      if (encoder.encode(pubkey).byteLength !== 64) {
        return [errorDelivery(peer.id, "bad_request", "public key must be 64 hex characters")];
      }
      if (!validKey(pubkey)) {
        return [errorDelivery(peer.id, "bad_request", "public key is not valid hex")];
      }
      peer.pubkey = pubkey;
    } else if (pubkey !== "" && pubkey !== peer.pubkey) {
      return [errorDelivery(peer.id, "bad_request", "public key cannot change during a session")];
    }
    if (peer.lobby || peer.code) return [{ id: peer.id, frame: this.#roster(peer) }];
    const key = `network:${peer.network}`;
    let room = this.#rooms.get(key);
    let admissionError;
    if (!room && this.#rooms.size >= MAX_ROOMS) admissionError = "capacity";
    else if (room && room.members.size >= MAX_PEERS) admissionError = "network_busy";
    const joined = { id: peer.id, frame: { type: "joined", peer_id: peer.id } };
    if (admissionError) return [joined, errorDelivery(peer.id, admissionError)];
    if (!room) {
      room = this.#newRoom(peer.network, "");
      this.#rooms.set(key, room);
    }
    room.members.add(peer.id);
    peer.lobby = true;
    return [joined, ...this.#rosters(room)];
  }

  #create(peer) {
    if (peer.code) return [errorDelivery(peer.id, "already_in_room")];
    if (!peer.pubkey) return [errorDelivery(peer.id, "not_in_room")];
    if (!this.#allowCreate(peer.network)) return [errorDelivery(peer.id, "rate_limited")];
    if (this.#rooms.size >= MAX_ROOMS) return [errorDelivery(peer.id, "capacity")];
    let code;
    try {
      for (let attempt = 0; attempt < 8; attempt++) {
        const candidate = this.#newCode();
        if (!this.#rooms.has(`code:${candidate}`)) {
          code = candidate;
          break;
        }
      }
    } catch (err) {
      return [errorDelivery(peer.id, "bad_request", `generate room code: ${err.message}`)];
    }
    if (!code) return [errorDelivery(peer.id, "capacity")];
    const room = this.#newRoom(peer.network, code);
    room.members.add(peer.id);
    this.#rooms.set(`code:${code}`, room);
    const previous = this.#depart(peer);
    peer.lobby = false;
    peer.code = code;
    return [
      ...previous,
      { id: peer.id, frame: { type: "created", code, peer_id: peer.id } },
      ...this.#rosters(room),
    ];
  }

  #join(peer, rawCode) {
    if (peer.code) return [errorDelivery(peer.id, "already_in_room")];
    if (!peer.pubkey) return [errorDelivery(peer.id, "not_in_room")];
    const code = normalizeCode(rawCode);
    const room = this.#rooms.get(`code:${code}`);
    if (!room) return [errorDelivery(peer.id, "no_room")];
    if (room.network !== peer.network) return [errorDelivery(peer.id, "network_mismatch")];
    if (room.members.size >= MAX_PEERS) return [errorDelivery(peer.id, "room_full")];
    room.members.add(peer.id);
    const previous = this.#depart(peer);
    peer.lobby = false;
    peer.code = code;
    return [
      ...previous,
      { id: peer.id, frame: { type: "joined", code, peer_id: peer.id } },
      ...this.#rosters(room),
    ];
  }

  #signal(peer, to, payload) {
    if (!peer.pubkey) return [errorDelivery(peer.id, "not_in_room")];
    if (!payload) return [errorDelivery(peer.id, "bad_request", "signal frame has empty payload")];
    if (encoder.encode(payload).byteLength > MAX_SIGNAL_BYTES) {
      return [errorDelivery(peer.id, "bad_request", "signal payload is too large")];
    }
    const room = this.#rooms.get(this.#roomKey(peer));
    if (to === peer.id || !room?.members.has(to)) return [errorDelivery(peer.id, "no_peer")];
    return [{ id: to, frame: { type: "signal", from: peer.id, payload } }];
  }

  #roomKey(peer) {
    return peer.code ? `code:${peer.code}` : peer.lobby ? `network:${peer.network}` : "";
  }

  #newRoom(network, code) {
    return { network, code, members: new Set() };
  }

  #roster(peer) {
    const frame = { type: "roster" };
    if (peer.code) frame.code = peer.code;
    const room = this.#rooms.get(this.#roomKey(peer));
    if (room) {
      const peers = [];
      for (const id of room.members) {
        if (id === peer.id) continue;
        const other = this.#peers.get(id);
        peers.push({ id, pubkey: other.pubkey, source: room.code ? "code" : "network" });
      }
      // Match Go's omitempty: an empty roster has no peers field.
      if (peers.length) frame.peers = peers;
    }
    return frame;
  }

  #rosters(room) {
    return [...room.members].map((id) => ({ id, frame: this.#roster(this.#peers.get(id)) }));
  }

  #depart(peer) {
    const key = this.#roomKey(peer);
    const room = this.#rooms.get(key);
    if (!room) return [];
    room.members.delete(peer.id);
    if (room.members.size === 0) {
      this.#rooms.delete(key);
      return [];
    }
    const deliveries = [];
    for (const id of room.members) {
      deliveries.push({ id, frame: { type: "peer_left", peer_id: peer.id } });
      deliveries.push({ id, frame: this.#roster(this.#peers.get(id)) });
    }
    return deliveries;
  }

  #allowCreate(network) {
    const now = this.#now();
    const window = this.#rates.get(network);
    if (!window || now - window.start >= MINUTE) {
      this.#rates.set(network, { start: now, count: 1 });
      return true;
    }
    if (window.count >= CREATE_PER_MINUTE) return false;
    window.count++;
    return true;
  }

  #sweepRates(now) {
    for (const [network, window] of this.#rates) {
      if (now - window.start >= 2 * MINUTE) this.#rates.delete(network);
    }
  }

  #newCode() {
    let bare = "";
    while (bare.length < 6) {
      const bytes = this.#randomBytes(6 - bare.length);
      if (!(bytes instanceof Uint8Array) || bytes.length !== 6 - bare.length) {
        throw new TypeError("random source returned invalid bytes");
      }
      for (const byte of bytes) {
        // Rejection sampling avoids modulo bias in the 27-symbol alphabet.
        if (byte < 243) bare += CODE_ALPHABET[byte % CODE_ALPHABET.length];
      }
    }
    return `${bare.slice(0, 3)}-${bare.slice(3)}`;
  }
}
