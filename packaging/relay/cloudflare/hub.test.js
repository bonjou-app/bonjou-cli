import assert from "node:assert/strict";
import test from "node:test";

import { Coordinator } from "./hub.js";

const KEY = "ab".repeat(32);
const OTHER_KEY = "cd".repeat(32);
const id = (value) => value.toString(16).padStart(16, "0");
const fixedRandom = (length) => new Uint8Array(length);

function connected(hub, value, network = "network-a", key = KEY) {
  const peerID = id(value);
  assert.deepEqual(hub.addPeer({ id: peerID, network }), []);
  hub.message(peerID, { type: "hello", pubkey: key });
  return peerID;
}

function framesFor(deliveries, peerID) {
  return deliveries.filter(({ id: target }) => target === peerID).map(({ frame }) => frame);
}

function errorCode(deliveries) {
  return deliveries.find(({ frame }) => frame.type === "error")?.frame.code_error;
}

function create(hub, peerID) {
  const deliveries = hub.message(peerID, { type: "create" });
  const code = deliveries.find(({ frame }) => frame.type === "created")?.frame.code;
  assert.ok(code, JSON.stringify(deliveries));
  return { code, deliveries };
}

test("new peers remain pending and retired connections cannot mutate state", () => {
  const hub = new Coordinator();
  const alice = id(1);
  assert.deepEqual(hub.addPeer({ id: alice, network: "network-a" }), []);
  assert.equal(hub.roomCount, 0);
  assert.deepEqual(hub.exportPeer(alice), {
    version: 1, id: alice, network: "network-a", pubkey: "", code: "", lobby: false,
  });
  assert.throws(() => hub.addPeer({ id: alice, network: "network-b" }), TypeError);
  assert.throws(() => hub.addPeer({ id: id(2), network: "" }), TypeError);
  assert.deepEqual(hub.removePeer(alice), []);
  assert.equal(hub.exportPeer(alice), undefined);
  assert.deepEqual(hub.message(alice, { type: "create" }), []);
  assert.deepEqual(hub.removePeer(alice), []);
});

test("hello validates UTF-8 key length and preserves the exact public key", () => {
  const hub = new Coordinator();
  const alice = id(1);
  hub.addPeer({ id: alice, network: "network-a" });
  for (const pubkey of ["", "a".repeat(63), "é".repeat(64)]) {
    assert.deepEqual(framesFor(hub.message(alice, { type: "hello", pubkey }), alice), [{
      type: "error", code_error: "bad_request", message: "public key must be 64 hex characters",
    }]);
  }
  assert.equal(hub.exportPeer(alice).pubkey, "");
  assert.deepEqual(framesFor(hub.message(alice, { type: "hello", pubkey: "z".repeat(64) }), alice), [{
    type: "error", code_error: "bad_request", message: "public key is not valid hex",
  }]);
  assert.deepEqual(framesFor(hub.message(alice, { type: "hello", pubkey: KEY }), alice), [
    { type: "joined", peer_id: alice }, { type: "roster" },
  ]);
  assert.equal(hub.exportPeer(alice).pubkey, KEY);
  assert.equal(hub.exportPeer(alice).lobby, true);
  assert.equal(hub.roomCount, 1);
});

test("lobby rosters expose only same-network session keys and exclude self", () => {
  const hub = new Coordinator();
  const alice = connected(hub, 1);
  const outsider = connected(hub, 3, "network-b");
  const bob = id(2);
  hub.addPeer({ id: bob, network: "network-a" });
  const deliveries = hub.message(bob, { type: "hello", pubkey: OTHER_KEY, name: "ignored profile" });
  assert.deepEqual(framesFor(deliveries, alice), [{
    type: "roster", peers: [{ id: bob, pubkey: OTHER_KEY, source: "network" }],
  }]);
  assert.deepEqual(framesFor(deliveries, bob), [
    { type: "joined", peer_id: bob },
    { type: "roster", peers: [{ id: alice, pubkey: KEY, source: "network" }] },
  ]);
  assert.deepEqual(framesFor(deliveries, outsider), []);
  assert.deepEqual(hub.message(alice, { type: "hello" }), [{
    id: alice, frame: { type: "roster", peers: [{ id: bob, pubkey: OTHER_KEY, source: "network" }] },
  }]);
  assert.equal(JSON.stringify(hub.exportPeer(bob)).includes("ignored profile"), false);
});

test("a session cannot change its public key, including changing hex case", () => {
  const hub = new Coordinator();
  const alice = connected(hub, 1);
  const before = hub.exportPeer(alice);
  for (const pubkey of [OTHER_KEY, KEY.toUpperCase()]) {
    assert.deepEqual(hub.message(alice, { type: "hello", pubkey }), [{
      id: alice, frame: {
        type: "error", code_error: "bad_request", message: "public key cannot change during a session",
      },
    }]);
    assert.deepEqual(hub.exportPeer(alice), before);
  }
});

test("room and signal actions require hello without making protocol errors fatal", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = id(1);
  hub.addPeer({ id: alice, network: "network-a" });
  for (const frame of [{ type: "create" }, { type: "join", code: "222-222" }, {
    type: "signal", to: id(2), payload: "opaque",
  }]) {
    assert.equal(errorCode(hub.message(alice, frame)), "not_in_room");
  }
  assert.equal(hub.roomCount, 0);
  hub.message(alice, { type: "hello", pubkey: KEY });
  assert.equal(create(hub, alice).code, "222-222");
});

test("code rooms narrow visibility and block signaling across the lobby boundary", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(hub, 1);
  const bob = connected(hub, 2);
  const carol = connected(hub, 3);
  const { code, deliveries } = create(hub, alice);
  assert.deepEqual(framesFor(deliveries, bob), [
    { type: "peer_left", peer_id: alice },
    { type: "roster", peers: [{ id: carol, pubkey: KEY, source: "network" }] },
  ]);
  assert.deepEqual(framesFor(deliveries, alice), [
    { type: "created", code, peer_id: alice }, { type: "roster", code },
  ]);
  assert.equal(errorCode(hub.message(alice, { type: "signal", to: bob, payload: "opaque" })), "no_peer");
  assert.equal(errorCode(hub.message(bob, { type: "signal", to: alice, payload: "opaque" })), "no_peer");
  const joined = hub.message(bob, { type: "join", code });
  assert.deepEqual(framesFor(joined, carol), [
    { type: "peer_left", peer_id: bob }, { type: "roster" },
  ]);
  assert.deepEqual(framesFor(joined, alice), [{
    type: "roster", code, peers: [{ id: bob, pubkey: KEY, source: "code" }],
  }]);
  assert.deepEqual(framesFor(joined, bob), [
    { type: "joined", code, peer_id: bob },
    { type: "roster", code, peers: [{ id: alice, pubkey: KEY, source: "code" }] },
  ]);
  assert.equal(errorCode(hub.message(bob, { type: "create" })), "already_in_room");
  assert.equal(errorCode(hub.message(bob, { type: "join", code })), "already_in_room");
  assert.equal(hub.exportPeer(alice).lobby, false);
  assert.equal(hub.exportPeer(bob).lobby, false);
});

test("foreign rooms return network_mismatch and failed joins preserve lobby membership", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(hub, 1);
  const outsider = connected(hub, 2, "network-b");
  const before = hub.exportPeer(outsider);
  const { code } = create(hub, alice);
  assert.deepEqual(hub.message(outsider, { type: "join", code }), [{
    id: outsider, frame: {
      type: "error", code_error: "network_mismatch", message: "that room belongs to a different network",
    },
  }]);
  assert.deepEqual(hub.exportPeer(outsider), before);
  assert.equal(errorCode(hub.message(outsider, { type: "join", code: "BBB-CCC" })), "no_room");
  assert.equal(errorCode(hub.message(outsider, { type: "signal", to: alice, payload: "opaque" })), "no_peer");
});

test("normalization matches Go's dropped glyphs and simple Unicode uppercase", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(hub, 1);
  const { code } = create(hub, alice);
  let counter = 2;
  for (const rawCode of [" 222 222 ", "0 O o I i L l 222-222", "ß222-222", "222---222"]) {
    const peer = connected(hub, counter++);
    assert.equal(errorCode(hub.message(peer, { type: "join", code: rawCode })), undefined);
    assert.equal(hub.exportPeer(peer).code, code);
  }
  for (const rawCode of ["", "22222", "2222222", "222-22O", "net:network-a"]) {
    const peer = connected(hub, counter++);
    assert.equal(errorCode(hub.message(peer, { type: "join", code: rawCode })), "no_room");
    assert.equal(hub.exportPeer(peer).lobby, true);
  }
});

test("departure notifies current scope only and last-peer retirement invalidates the code", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(hub, 1);
  const bob = connected(hub, 2);
  const carol = connected(hub, 3);
  const { code } = create(hub, alice);
  hub.message(bob, { type: "join", code });
  const before = hub.exportPeer(alice);
  const departed = hub.removePeer(bob);
  assert.deepEqual(departed, [
    { id: alice, frame: { type: "peer_left", peer_id: bob } },
    { id: alice, frame: { type: "roster", code } },
  ]);
  assert.deepEqual(hub.exportPeer(alice), before);
  assert.deepEqual(framesFor(departed, carol), []);
  assert.deepEqual(hub.removePeer(alice), []);
  assert.equal(hub.roomCount, 1);
  assert.equal(errorCode(hub.message(carol, { type: "join", code })), "no_room");
  assert.equal(create(hub, carol).code, code);
  hub.removePeer(carol);
  assert.equal(hub.roomCount, 0);
});

test("signals remain opaque, respect UTF-8 byte limits, and never route to self", () => {
  const hub = new Coordinator();
  const alice = connected(hub, 1);
  const bob = connected(hub, 2);
  const opaque = "\u0000\"opaque ☃ ciphertext\"\n";
  assert.deepEqual(hub.message(alice, { type: "signal", to: bob, payload: opaque }), [{
    id: bob, frame: { type: "signal", from: alice, payload: opaque },
  }]);
  assert.equal(JSON.stringify(hub.exportPeer(alice)).includes("ciphertext"), false);
  assert.equal(JSON.stringify(hub.exportRates()).includes("ciphertext"), false);
  assert.equal(errorCode(hub.message(alice, { type: "signal", to: alice, payload: opaque })), "no_peer");
  assert.equal(errorCode(hub.message(alice, { type: "signal", to: id(99), payload: opaque })), "no_peer");
  assert.equal(errorCode(hub.message(alice, { type: "signal", to: bob, payload: "" })), "bad_request");
  const exact = "é".repeat((96 << 10) / 2);
  assert.equal(hub.message(alice, { type: "signal", to: bob, payload: exact })[0].frame.payload, exact);
  assert.deepEqual(hub.message(alice, { type: "signal", to: bob, payload: `${exact}é` }), [{
    id: alice, frame: { type: "error", code_error: "bad_request", message: "signal payload is too large" },
  }]);
});

test("application and legacy payload message kinds are rejected without retention", () => {
  const hub = new Coordinator();
  const alice = connected(hub, 1);
  for (const type of ["relay", "transfer_begin", "transfer_end"]) {
    assert.equal(errorCode(hub.message(alice, { type, payload: "ignored content" })), "unsupported_message");
  }
  for (const type of ["profile", "chat", "file", "file_metadata", "file_payload"]) {
    assert.equal(errorCode(hub.message(alice, { type, payload: "ignored content" })), "bad_request");
  }
  assert.equal(JSON.stringify(hub.exportPeer(alice)).includes("ignored content"), false);
});

test("JSON fields follow Go's case matching, null handling, and fatal type validation", () => {
  const hub = new Coordinator();
  const alice = id(1);
  hub.addPeer({ id: alice, network: "network-a" });
  assert.equal(errorCode(hub.message(alice, null)), "bad_request");
  assert.equal(errorCode(hub.message(alice, { type: "hello", pubkey: null })), "bad_request");
  assert.deepEqual(framesFor(hub.message(alice, {
    TYPE: "hello", PUBKEY: KEY, profile: { nickname: "unused" }, payload: null,
  }), alice), [{ type: "joined", peer_id: alice }, { type: "roster" }]);
  assert.deepEqual(hub.message(alice, { type: "hello", pubkey: KEY, PubKey: null }), [{
    id: alice, frame: { type: "roster" },
  }]);
  for (const frame of [[], "hello", 1, false, { type: 1 }, { type: "hello", pubkey: {} }, {
    type: "hello", payload: 1,
  }]) assert.throws(() => hub.message(alice, frame), TypeError);
  assert.equal(hub.exportPeer(alice).pubkey, KEY);
  assert.equal(JSON.stringify(hub.exportPeer(alice)).includes("unused"), false);
});

test("rehydration reconstructs lobby and code isolation while pending peers remain pending", () => {
  const original = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(original, 1);
  const bob = connected(original, 2);
  const carol = connected(original, 3);
  const outsider = connected(original, 4, "network-b");
  const pending = id(5);
  original.addPeer({ id: pending, network: "network-a" });
  const { code } = create(original, alice);
  original.message(bob, { type: "join", code });
  const restored = new Coordinator();
  for (const peerID of [bob, pending, outsider, carol, alice]) {
    assert.deepEqual(restored.restorePeer(original.exportPeer(peerID)), []);
    assert.deepEqual(restored.exportPeer(peerID), original.exportPeer(peerID));
  }
  assert.equal(restored.roomCount, 3);
  assert.deepEqual(restored.message(alice, { type: "hello" }), [{
    id: alice, frame: { type: "roster", code, peers: [{ id: bob, pubkey: KEY, source: "code" }] },
  }]);
  assert.deepEqual(restored.message(carol, { type: "hello" }), [{ id: carol, frame: { type: "roster" } }]);
  assert.equal(errorCode(restored.message(outsider, { type: "join", code })), "network_mismatch");
  assert.equal(errorCode(restored.message(pending, { type: "create" })), "not_in_room");
  assert.equal(errorCode(restored.message(alice, { type: "signal", to: carol, payload: "opaque" })), "no_peer");
  assert.deepEqual(restored.message(alice, { type: "signal", to: bob, payload: "opaque" }), [{
    id: bob, frame: { type: "signal", from: alice, payload: "opaque" },
  }]);
});

test("malformed and cross-network conflicting attachments fail without changing the hub", () => {
  const hub = new Coordinator();
  const base = { version: 1, id: id(1), network: "network-a", pubkey: KEY, code: "222-222", lobby: false };
  hub.restorePeer(base);
  for (const patch of [
    { version: 2 }, { pubkey: "invalid" }, { pubkey: "" }, { code: "222222" },
    { lobby: true }, { lobby: undefined }, { network: "" },
  ]) assert.throws(() => hub.restorePeer({ ...base, id: id(2), ...patch }), TypeError);
  assert.throws(() => hub.restorePeer({ ...base, id: id(2), network: "network-b" }), TypeError);
  assert.equal(hub.exportPeer(id(2)), undefined);
  assert.equal(hub.roomCount, 1);
  const snapshot = hub.exportPeer(id(1));
  snapshot.pubkey = OTHER_KEY;
  snapshot.code = "BBB-CCC";
  assert.deepEqual(hub.exportPeer(id(1)), base);
});

test("the 129th lobby peer is rejected and restoration never promotes a rejected peer", () => {
  const hub = new Coordinator();
  const members = [];
  for (let value = 1; value <= 128; value++) members.push(connected(hub, value));
  const overflow = id(129);
  hub.addPeer({ id: overflow, network: "network-a" });
  assert.deepEqual(framesFor(hub.message(overflow, { type: "hello", pubkey: KEY }), overflow), [
    { type: "joined", peer_id: overflow },
    {
      type: "error", code_error: "network_busy",
      message: "too many devices share this network address to group them safely",
    },
  ]);
  assert.equal(hub.exportPeer(overflow).lobby, false);
  assert.equal(hub.exportPeer(overflow).pubkey, KEY);
  const restored = new Coordinator();
  restored.restorePeer(hub.exportPeer(overflow));
  for (const member of members.reverse()) restored.restorePeer(hub.exportPeer(member));
  assert.equal(restored.exportPeer(overflow).lobby, false);
  assert.equal(errorCode(restored.message(overflow, { type: "hello" })), "network_busy");
  assert.equal(errorCode(restored.message(overflow, { type: "signal", to: id(1), payload: "opaque" })), "no_peer");
  restored.removePeer(id(1));
  assert.equal(errorCode(restored.message(overflow, { type: "hello" })), undefined);
  assert.equal(restored.exportPeer(overflow).lobby, true);
});

test("code rooms admit 128 peers and full foreign rooms still return network_mismatch", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  const alice = connected(hub, 1);
  const { code } = create(hub, alice);
  for (let value = 2; value <= 128; value++) {
    const peer = connected(hub, value);
    assert.equal(errorCode(hub.message(peer, { type: "join", code })), undefined);
  }
  const overflow = connected(hub, 129);
  assert.equal(errorCode(hub.message(overflow, { type: "join", code })), "room_full");
  assert.equal(hub.exportPeer(overflow).lobby, true);
  const outsider = connected(hub, 130, "network-b");
  assert.equal(errorCode(hub.message(outsider, { type: "join", code })), "network_mismatch");
  hub.removePeer(alice);
  assert.equal(errorCode(hub.message(overflow, { type: "join", code })), undefined);
});

test("the global capacity counts lobbies and preserves admission state during recovery", () => {
  const hub = new Coordinator({ randomBytes: fixedRandom });
  for (let value = 1; value <= 5000; value++) {
    hub.restorePeer({
      version: 1, id: id(value), network: `network-${value}`, pubkey: KEY, code: "", lobby: true,
    });
  }
  assert.equal(hub.roomCount, 5000);
  const extra = id(5001);
  hub.addPeer({ id: extra, network: "network-extra" });
  assert.equal(errorCode(hub.message(extra, { type: "hello", pubkey: KEY })), "capacity");
  assert.equal(hub.exportPeer(extra).lobby, false);
  assert.equal(errorCode(hub.message(extra, { type: "create" })), "capacity");
  assert.deepEqual(hub.exportRates().map(({ network, count }) => ({ network, count })), [{
    network: "network-extra", count: 1,
  }]);
  hub.removePeer(id(1));
  assert.equal(hub.roomCount, 4999);
  assert.equal(errorCode(hub.message(extra, { type: "hello" })), undefined);
  assert.equal(hub.roomCount, 5000);
  assert.equal(hub.exportPeer(extra).lobby, true);
  assert.equal(errorCode(hub.message(extra, { type: "create" })), "capacity");
});

test("creation rates survive disconnects and rehydration and reset at the minute boundary", () => {
  let now = 1000;
  const hub = new Coordinator({ now: () => now, randomBytes: fixedRandom });
  for (let value = 1; value <= 20; value++) {
    const peer = connected(hub, value);
    create(hub, peer);
    hub.removePeer(peer);
  }
  assert.equal(hub.roomCount, 0);
  const overflow = connected(hub, 21);
  assert.equal(errorCode(hub.message(overflow, { type: "create" })), "rate_limited");
  assert.deepEqual(hub.exportRates(), [{ network: "network-a", start: 1000, count: 20 }]);
  let nextCode = 0;
  const restored = new Coordinator({
    now: () => now,
    randomBytes: (length) => new Uint8Array(length).fill(nextCode++),
  });
  restored.restorePeer(hub.exportPeer(overflow));
  restored.restoreRates(hub.exportRates());
  const savedRates = restored.exportRates();
  savedRates[0].count = 1;
  assert.equal(errorCode(restored.message(overflow, { type: "create" })), "rate_limited");
  const otherNetwork = connected(restored, 22, "network-b");
  create(restored, otherNetwork);
  now = 60_999;
  assert.equal(errorCode(restored.message(overflow, { type: "create" })), "rate_limited");
  now = 61_000;
  create(restored, overflow);
  assert.equal(restored.exportRates().find(({ network }) => network === "network-a").count, 1);
  now = 181_000;
  assert.deepEqual(restored.exportRates(), []);
  assert.equal(restored.roomCount, 2, "occupied rooms do not expire from signaling inactivity");
});

test("rate restoration rejects malformed snapshots atomically and drops aged windows", () => {
  const hub = new Coordinator({ now: () => 121_000 });
  const current = [{ network: "network-a", start: 120_000, count: 2 }];
  hub.restoreRates(current);
  for (const rates of [
    null, [{ network: "network-b", start: 120_000, count: 21 }],
    [{ network: "network-b", start: NaN, count: 1 }],
    [{ network: "network-b", start: 120_000, count: 1.5 }],
    [...current, ...current],
  ]) {
    assert.throws(() => hub.restoreRates(rates), TypeError);
    assert.deepEqual(hub.exportRates(), current);
  }
  hub.restoreRates([...current, { network: "old-network", start: 1000, count: 20 }]);
  assert.deepEqual(hub.exportRates(), current);
});

test("global room-code collisions are retried eight times without moving the failing peer", () => {
  let calls = 0;
  const hub = new Coordinator({ randomBytes(length) { calls++; return fixedRandom(length); } });
  const alice = connected(hub, 1);
  const bob = connected(hub, 2, "network-b");
  create(hub, alice);
  const before = hub.exportPeer(bob);
  assert.equal(errorCode(hub.message(bob, { type: "create" })), "capacity");
  assert.equal(calls, 9, "one successful allocation followed by eight collision attempts");
  assert.deepEqual(hub.exportPeer(bob), before);
  assert.equal(hub.roomCount, 2);
});

test("room codes use rejection sampling and entropy failures do not publish a room", () => {
  let first = true;
  const hub = new Coordinator({ randomBytes(length) {
    if (first) { first = false; return new Uint8Array(length).fill(255); }
    return new Uint8Array(length).fill(27);
  } });
  const alice = connected(hub, 1);
  assert.equal(create(hub, alice).code, "222-222");
  const failing = new Coordinator({ randomBytes() { throw new Error("entropy unavailable"); } });
  const bob = connected(failing, 2);
  assert.deepEqual(failing.message(bob, { type: "create" }), [{
    id: bob, frame: { type: "error", code_error: "bad_request", message: "generate room code: entropy unavailable" },
  }]);
  assert.equal(failing.roomCount, 1);
  assert.equal(failing.exportPeer(bob).lobby, true);
});
