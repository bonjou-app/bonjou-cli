import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { after, before, test } from "node:test";
import { Miniflare, convertV4MiniflareOptions } from "miniflare";

const origin = "https://bonjou.vercel.app";
const base = "https://coordinator.example";
const key = (digit) => digit.repeat(64);
const timeoutMs = 3000;
let runtime;
let runtimeOptions;

before(async () => {
  const config = JSON.parse(await readFile(new URL("./wrangler.jsonc", import.meta.url), "utf8"));
  const binding = config.durable_objects.bindings.find((entry) => entry.name === "COORDINATOR_HUB");
  assert.equal(binding.class_name, "BonjouCoordinatorHub");
  assert.ok(config.migrations.some((migration) => migration.new_sqlite_classes?.includes(binding.class_name)));
  runtimeOptions = {
    modules: [config.main, "hub.js", "network.js"].map((name) => ({
      type: "ESModule",
      path: fileURLToPath(new URL(`./${name}`, import.meta.url)),
    })),
    modulesRoot: fileURLToPath(new URL(".", import.meta.url)),
    compatibilityDate: config.compatibility_date,
    compatibilityFlags: config.compatibility_flags,
    bindings: { ...config.vars, ALLOWED_ORIGINS: origin },
    durableObjects: { COORDINATOR_HUB: { className: binding.class_name, useSQLite: true } },
    port: 0,
  };
  // Wrangler's pinned Miniflare 5 exposes the supported v4 option converter.
  runtime = new Miniflare(convertV4MiniflareOptions(runtimeOptions));
});

after(async () => {
  await runtime?.dispose();
});

async function request(path, headers = {}, method = "GET") {
  return runtime.dispatchFetch(`${base}${path}`, { method, headers });
}

async function connect(t, ip, extraHeaders = {}) {
  const response = await request("/ws", {
    Upgrade: "websocket",
    Origin: origin,
    "CF-Connecting-IP": ip,
    ...extraHeaders,
  });
  assert.equal(response.status, 101, `WebSocket handshake: ${response.status}`);
  const socket = response.webSocket;
  assert.ok(socket);
  const queued = [];
  const pending = new Set();
  let closeEvent;
  let resolveClose;
  const closed = new Promise((resolve) => { resolveClose = resolve; });
  socket.addEventListener("message", (event) => {
    const frame = JSON.parse(event.data);
    const waiter = [...pending].find((entry) => entry.match(frame));
    if (waiter) {
      pending.delete(waiter);
      clearTimeout(waiter.timer);
      waiter.resolve(frame);
    } else {
      queued.push(frame);
    }
  });
  socket.addEventListener("close", (event) => {
    closeEvent = event;
    resolveClose(event);
    for (const waiter of pending) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error(`WebSocket closed (${event.code}) while waiting for ${waiter.label}`));
    }
    pending.clear();
  });
  socket.accept();
  const peer = {
    socket,
    id: "",
    send(frame) { socket.send(JSON.stringify(frame)); },
    raw(value) { socket.send(value); },
    expect(match, label) {
      const index = queued.findIndex(match);
      if (index !== -1) return Promise.resolve(queued.splice(index, 1)[0]);
      if (closeEvent) return Promise.reject(new Error(`WebSocket already closed (${closeEvent.code})`));
      return new Promise((resolve, reject) => {
        const waiter = { match, label, resolve, reject };
        waiter.timer = setTimeout(() => {
          pending.delete(waiter);
          reject(new Error(`Timed out waiting for ${label}`));
        }, timeoutMs);
        pending.add(waiter);
      });
    },
    async close() {
      if (!closeEvent && socket.readyState === 1) socket.close(1000, "test complete");
      return peer.waitClosed();
    },
    async waitClosed() {
      return Promise.race([
        closed,
        new Promise((_, reject) => {
          const timer = setTimeout(() => reject(new Error("WebSocket close did not complete")), timeoutMs);
          closed.finally(() => clearTimeout(timer));
        }),
      ]);
    },
  };
  t.after(() => peer.close());
  return peer;
}

async function hello(peer, publicKey) {
  peer.send({ type: "hello", pubkey: publicKey });
  const joined = await peer.expect((frame) => frame.type === "joined" && !frame.code, "lobby join");
  assert.match(joined.peer_id, /^[0-9a-f]{16}$/);
  peer.id = joined.peer_id;
}

async function error(peer, code) {
  const frame = await peer.expect((entry) => entry.type === "error", `error ${code}`);
  assert.equal(frame.code_error, code);
  assert.equal(typeof frame.message, "string");
  return frame;
}

async function create(peer, frame = { type: "create" }) {
  peer.send(frame);
  const created = await peer.expect((frame) => frame.type === "created", "room creation");
  assert.match(created.code, /^[23456789BCDFGHJKMNPQRSTVWXZ]{3}-[23456789BCDFGHJKMNPQRSTVWXZ]{3}$/);
  assert.equal(created.peer_id, peer.id);
  return created.code;
}

test("HTTP surface exposes health and no application payload route", async () => {
  const health = await request("/healthz");
  assert.equal(health.status, 200);
  const body = await health.json();
  assert.equal(body.status, "ok");
  assert.ok(!("transfers" in body));
  for (const path of ["/t/upload", "/t/download", "/transfer", "/", "/ws/extra"]) {
    const response = await request(path);
    assert.equal(response.status, 404, path);
  }
  const upload = await request("/t/upload", {}, "POST");
  assert.equal(upload.status, 404);
  const healthPost = await request("/healthz", {}, "POST");
  assert.equal(healthPost.status, 405);
});

test("handshake denies invalid origins and untrusted ingress metadata", async () => {
  const headers = { Upgrade: "websocket", Origin: origin, "CF-Connecting-IP": "192.0.2.1" };
  for (const badOrigin of [undefined, "https://attacker.example", `${origin}.attacker.example`, `${origin}/app`, "null"]) {
    const values = { ...headers };
    if (badOrigin === undefined) delete values.Origin;
    else values.Origin = badOrigin;
    const response = await request("/ws", values);
    assert.equal(response.status, 403, `origin ${badOrigin}`);
  }
  const noUpgrade = await request("/ws", { Origin: origin, "CF-Connecting-IP": "192.0.2.1" });
  assert.equal(noUpgrade.status, 426);
  const wrongMethod = await request("/ws", { Origin: origin, "CF-Connecting-IP": "192.0.2.1" }, "POST");
  assert.equal(wrongMethod.status, 405);
  // Miniflare supplies a connecting address for absent or empty headers, as
  // the edge does. Missing-metadata rejection is covered in network.test.js.
  for (const badIP of ["not-an-ip", "192.0.2.1, 198.51.100.1", "2a06:98c0:3600::103"]) {
    const values = { ...headers };
    values["CF-Connecting-IP"] = badIP;
    const response = await request("/ws", values);
    assert.ok(response.status >= 400 && response.status < 600, `IP ${badIP}: ${response.status}`);
    assert.equal(response.webSocket, null);
  }
  const subrequest = await request("/ws", { ...headers, "CF-Worker": "upstream.example" });
  assert.equal(subrequest.status, 503);
});

test("trusted network grouping survives forged forwarding and private headers", async (t) => {
  const alice = await connect(t, "192.0.2.10", { "X-Bonjou-Network": key("1") });
  const bob = await connect(t, "192.0.2.10", {
    "X-Forwarded-For": "198.51.100.20",
    "X-Real-IP": "198.51.100.20",
    "X-Bonjou-Network": key("2"),
  });
  const stranger = await connect(t, "198.51.100.20", { "X-Bonjou-Network": key("1") });
  await hello(alice, key("a"));
  await hello(bob, key("b"));
  await hello(stranger, key("c"));
  const roster = await alice.expect((frame) => frame.type === "roster" && frame.peers?.some((peer) => peer.id === bob.id), "same-network roster");
  assert.deepEqual(roster.peers.find((peer) => peer.id === bob.id), { id: bob.id, pubkey: key("b"), source: "network" });
  assert.ok(roster.peers.every((peer) => peer.id !== stranger.id));
  const code = await create(alice);
  stranger.send({ type: "join", code });
  await error(stranger, "network_mismatch");
  stranger.send({ type: "signal", to: alice.id, payload: "opaque" });
  await error(stranger, "no_peer");
});

test("room membership narrows routing and signals remain opaque", async (t) => {
  const alice = await connect(t, "192.0.2.11");
  const bob = await connect(t, "192.0.2.11");
  await hello(alice, key("a"));
  await hello(bob, key("b"));
  await alice.expect((frame) => frame.type === "roster" && frame.peers?.some((peer) => peer.id === bob.id), "initial lobby roster");
  const code = await create(alice);
  await bob.expect((frame) => frame.type === "peer_left" && frame.peer_id === alice.id, "lobby departure");
  bob.send({ type: "signal", to: alice.id, payload: "still in lobby" });
  await error(bob, "no_peer");
  bob.send({ type: "join", code: code.toLowerCase().replace("-", " ") });
  const joined = await bob.expect((frame) => frame.type === "joined" && frame.code, "normalized room join");
  assert.equal(joined.code, code);
  const roster = await alice.expect((frame) => frame.type === "roster" && frame.code === code && frame.peers?.some((peer) => peer.id === bob.id), "room roster");
  assert.equal(roster.peers.find((peer) => peer.id === bob.id).source, "code");
  const ciphertext = '{"v":2,"n":"synthetic","c":"opaque-雪-🚀"}';
  alice.send({ type: "signal", to: bob.id, payload: ciphertext, ignored_future_field: "not retained" });
  const signal = await bob.expect((frame) => frame.type === "signal", "opaque signaling frame");
  assert.deepEqual(signal, { type: "signal", from: alice.id, payload: ciphertext });
  for (const type of ["relay", "transfer_begin", "transfer_end"]) {
    alice.send({ type, payload: "application bytes" });
    await error(alice, "unsupported_message");
  }
});

test("protocol errors preserve a valid socket but malformed wire frames close it", async (t) => {
  const peer = await connect(t, "192.0.2.12");
  peer.send({ type: "create" });
  await error(peer, "not_in_room");
  peer.send({ type: "hello", pubkey: "invalid" });
  await error(peer, "bad_request");
  await hello(peer, key("a"));
  peer.send({ type: "hello", pubkey: key("b") });
  await error(peer, "bad_request");
  peer.raw("null");
  await error(peer, "bad_request");
  peer.send({ type: "profile", name: "should never be retained" });
  await error(peer, "bad_request");
  peer.send({ type: "signal", to: "unavailable", payload: "雪".repeat(32769) });
  await error(peer, "bad_request");
  // The same byte cap as Go: this is below 128 Ki UTF-16 code units but
  // exceeds 128 KiB on the wire. A JavaScript string-length check would miss it.
  peer.raw(JSON.stringify({ type: "signal", to: "unavailable", payload: "雪".repeat(45000) }));
  const closed = await peer.waitClosed();
  assert.equal(closed.code, 1009);
  const oversizedASCII = await connect(t, "192.0.2.18");
  oversizedASCII.raw(JSON.stringify({ type: "signal", payload: "x".repeat(128 * 1024) }));
  assert.equal((await oversizedASCII.waitClosed()).code, 1009);
  for (const raw of ["{invalid", "[]", JSON.stringify({ type: 42 }), new Uint8Array([1, 2, 3])]) {
    const malformed = await connect(t, "192.0.2.13");
    malformed.raw(raw);
    const event = await malformed.waitClosed();
    assert.equal(event.code, 1007);
  }
});

test("binary JSON controls retain Go compatibility and their wire byte cap", async (t) => {
  const peer = await connect(t, "192.0.2.17");
  peer.raw(new TextEncoder().encode(JSON.stringify({ type: "hello", pubkey: key("a") })));
  const joined = await peer.expect((frame) => frame.type === "joined", "binary JSON lobby join");
  assert.match(joined.peer_id, /^[0-9a-f]{16}$/);
  peer.raw(new TextEncoder().encode(JSON.stringify({ type: "signal", to: "unavailable", payload: "雪".repeat(45000) })));
  const closed = await peer.waitClosed();
  assert.equal(closed.code, 1009);
});

test("an occupied room survives departure and retires when its final peer leaves", async (t) => {
  const alice = await connect(t, "192.0.2.14");
  const bob = await connect(t, "192.0.2.14");
  const charlie = await connect(t, "192.0.2.14");
  await hello(alice, key("a"));
  await hello(bob, key("b"));
  await hello(charlie, key("c"));
  const code = await create(alice);
  bob.send({ type: "join", code });
  await bob.expect((frame) => frame.type === "joined" && frame.code === code, "Bob's room join");
  await alice.close();
  await bob.expect((frame) => frame.type === "peer_left" && frame.peer_id === alice.id, "room departure");
  charlie.send({ type: "join", code });
  await charlie.expect((frame) => frame.type === "joined" && frame.code === code, "occupied room remains joinable");
  await bob.close();
  await charlie.close();
  const late = await connect(t, "192.0.2.14");
  await hello(late, key("d"));
  late.send({ type: "join", code });
  await error(late, "no_room");
});

test("idle hibernatable WebSockets retain room routing without periodic application traffic", { timeout: 20000 }, async (t) => {
  const alice = await connect(t, "192.0.2.15");
  const bob = await connect(t, "192.0.2.15");
  await hello(alice, key("a"));
  await hello(bob, key("b"));
  const code = await create(alice);
  bob.send({ type: "join", code });
  await bob.expect((frame) => frame.type === "joined" && frame.code === code, "idle test room join");
  await alice.expect((frame) => frame.type === "roster" && frame.code === code && frame.peers?.some((peer) => peer.id === bob.id), "idle test room roster");
  // Cloudflare's normal hibernation interval is approximately ten seconds.
  // Miniflare has no documented Node eviction API; continuity alone does
  // not prove that this local runtime actually reconstructed the object.
  await new Promise((resolve) => setTimeout(resolve, 12000));
  alice.send({ type: "signal", to: bob.id, payload: "after-idle" });
  const frame = await bob.expect((entry) => entry.type === "signal", "signal after idle");
  assert.deepEqual(frame, { type: "signal", from: alice.id, payload: "after-idle" });
});

test("SQLite creation-rate windows survive a workerd restart", { timeout: 20000 }, async (t) => {
  for (let attempt = 0; attempt < 19; attempt += 1) {
    const peer = await connect(t, "192.0.2.16");
    await hello(peer, key("a"));
    await create(peer, { type: "create", TYPE: null });
    await peer.close();
  }
  // setOptions is Miniflare's documented runtime restart API. Every socket
  // is closed first; this tests durable rate metadata, not WS hibernation.
  await runtime.setOptions(convertV4MiniflareOptions({
    ...runtimeOptions,
    bindings: { ...runtimeOptions.bindings, TEST_RUNTIME_RESTART: "second-instance" },
  }));
  const twentieth = await connect(t, "192.0.2.16");
  await hello(twentieth, key("b"));
  await create(twentieth);
  await twentieth.close();
  const limited = await connect(t, "192.0.2.16");
  await hello(limited, key("c"));
  limited.send({ type: "create" });
  await error(limited, "rate_limited");
});
