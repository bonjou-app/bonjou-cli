import assert from "node:assert/strict";
import { registerHooks } from "node:module";
import { DatabaseSync } from "node:sqlite";
import { test } from "node:test";

import { Coordinator } from "./hub.js";

// Import the production module without rewriting its source or method bodies.
// Only the unavailable platform base class is replaced; this is a constructor
// restoration harness, not a claim that a real runtime eviction occurred.
const baseURL = "bonjou-test:cloudflare-workers";
const hooks = registerHooks({
  resolve(specifier, context, nextResolve) {
    return specifier === "cloudflare:workers"
      ? { url: baseURL, shortCircuit: true }
      : nextResolve(specifier, context);
  },
  load(url, context, nextLoad) {
    return url === baseURL
      ? {
        format: "module",
        source: "export class DurableObject { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }",
        shortCircuit: true,
      }
      : nextLoad(url, context);
  },
});
let BonjouCoordinatorHub;
try {
  ({ BonjouCoordinatorHub } = await import("./worker.js"));
} finally {
  hooks.deregister();
}

const network = "1".repeat(64);
const peerID = (value) => value.toString(16).padStart(16, "0");
const publicKey = (id) => id.repeat(4);

function addPeer(hub, number, hello = true) {
  const id = peerID(number);
  hub.addPeer({ id, network });
  return {
    id,
    deliveries: hello ? hub.message(id, { type: "hello", pubkey: publicKey(id) }) : [],
  };
}

function socket(attachment, readyState = WebSocket.OPEN, initialFrames = []) {
  let stored = structuredClone(attachment);
  return {
    readyState,
    frames: structuredClone(initialFrames),
    closes: [],
    deserializeAttachment() { return structuredClone(stored); },
    serializeAttachment(value) { stored = structuredClone(value); },
    send(value) {
      assert.equal(this.readyState, WebSocket.OPEN);
      this.frames.push(JSON.parse(value));
    },
    close(code, reason) {
      this.closes.push({ code, reason });
      this.readyState = WebSocket.CLOSED;
    },
  };
}

function context(t, sockets) {
  const database = new DatabaseSync(":memory:");
  t.after(() => database.close());
  return {
    getWebSockets: () => sockets,
    storage: {
      // Execute the production SQL against Node's built-in SQLite instead
      // of matching or copying specific SQL strings into a fake database.
      sql: {
        exec(query, ...parameters) {
          const rows = database.prepare(query).all(...parameters);
          return {
            toArray: () => rows,
            one() {
              assert.equal(rows.length, 1);
              return rows[0];
            },
          };
        },
      },
    },
  };
}

test("constructor refresh clears departed and closing ghosts after every survivor is restored", (t) => {
  const prior = new Coordinator();
  const alice = addPeer(prior, 1).id;
  const bob = addPeer(prior, 2).id;
  const departed = addPeer(prior, 3).id;
  const closing = addPeer(prior, 4).id;
  const creation = prior.message(alice, { type: "create" });
  const code = creation.find(({ frame }) => frame.type === "created").frame.code;
  for (const id of [bob, departed, closing]) prior.message(id, { type: "join", code });
  const staleRoster = (id) => prior.message(id, { type: "hello" })[0].frame;
  const aliceSocket = socket(prior.exportPeer(alice), WebSocket.OPEN, [staleRoster(alice)]);
  const bobSocket = socket(prior.exportPeer(bob), WebSocket.OPEN, [staleRoster(bob)]);
  const closingSocket = socket(prior.exportPeer(closing), WebSocket.CLOSING);
  assert.equal(aliceSocket.frames[0].peers.length, 3);
  // The departed socket is already absent; the closing socket is still
  // listed by the platform. Neither should survive reconstruction.
  const restored = new BonjouCoordinatorHub(context(t, [aliceSocket, bobSocket, closingSocket]), {});
  assert.equal(restored.sockets.size, 2);
  assert.equal(restored.hub.roomCount, 1);
  for (const [own, peer, live] of [[alice, bob, aliceSocket], [bob, alice, bobSocket]]) {
    assert.equal(live.frames.length, 2, "one refresh follows the stale client roster");
    assert.deepEqual(live.frames[1], {
      type: "roster",
      code,
      peers: [{ id: peer, pubkey: publicKey(peer), source: "code" }],
    });
    assert.equal(restored.hub.exportPeer(own).code, code);
  }
  assert.deepEqual(closingSocket.frames, []);
  restored.webSocketClose(closingSocket);
  assert.equal(aliceSocket.frames.length, 2, "late close callback does not resurrect or disturb departed members");
  assert.equal(bobSocket.frames.length, 2);
});

test("constructor roster refresh does not readmit pending or capacity-rejected peers", (t) => {
  const prior = new Coordinator();
  const admitted = [];
  for (let number = 1; number <= 128; number += 1) admitted.push(addPeer(prior, number).id);
  const rejected = addPeer(prior, 129);
  assert.ok(rejected.deliveries.some(({ frame }) => frame.code_error === "network_busy"));
  const pending = addPeer(prior, 130, false).id;
  // One admitted socket has vanished and another is closing. Capacity is
  // now available, so accidentally calling hello for rejected peers would
  // promote them into the lobby and expose them in the survivor roster.
  const departed = admitted.pop();
  const closing = admitted.pop();
  const live = admitted.map((id) => socket(prior.exportPeer(id)));
  const rejectedSocket = socket(prior.exportPeer(rejected.id));
  const pendingSocket = socket(prior.exportPeer(pending));
  const closingSocket = socket(prior.exportPeer(closing), WebSocket.CLOSING);
  const restored = new BonjouCoordinatorHub(context(t, [...live, rejectedSocket, pendingSocket, closingSocket]), {});
  assert.equal(restored.sockets.size, 128);
  for (const candidate of live) {
    assert.equal(candidate.frames.length, 1);
    const frame = candidate.frames[0];
    assert.equal(frame.type, "roster");
    assert.equal(frame.peers.length, 125);
    assert.ok(frame.peers.every(({ id, source }) => admitted.includes(id) && source === "network"));
    assert.ok(frame.peers.every(({ id }) => ![departed, closing, rejected.id, pending].includes(id)));
  }
  assert.deepEqual(rejectedSocket.frames, []);
  assert.deepEqual(pendingSocket.frames, []);
  assert.deepEqual(closingSocket.frames, []);
  assert.deepEqual(restored.hub.exportPeer(rejected.id), prior.exportPeer(rejected.id));
  assert.deepEqual(restored.hub.exportPeer(pending), prior.exportPeer(pending));
});

test("a closing final room member cannot revive the room during constructor restoration", (t) => {
  const prior = new Coordinator();
  const departed = addPeer(prior, 1).id;
  const creation = prior.message(departed, { type: "create" });
  const code = creation.find(({ frame }) => frame.type === "created").frame.code;
  const pending = addPeer(prior, 2, false).id;
  const closingSocket = socket(prior.exportPeer(departed), WebSocket.CLOSING);
  const pendingSocket = socket(prior.exportPeer(pending));
  const restored = new BonjouCoordinatorHub(context(t, [closingSocket, pendingSocket]), {});
  assert.equal(restored.hub.roomCount, 0);
  assert.deepEqual(pendingSocket.frames, []);
  restored.webSocketMessage(pendingSocket, JSON.stringify({ type: "hello", pubkey: publicKey(pending) }));
  restored.webSocketMessage(pendingSocket, JSON.stringify({ type: "join", code }));
  assert.equal(pendingSocket.frames.at(-1).code_error, "no_room");
  assert.deepEqual(closingSocket.frames, []);
});
