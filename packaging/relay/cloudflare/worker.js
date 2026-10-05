import { DurableObject } from "cloudflare:workers";

import { Coordinator } from "./hub.js";
import { hashNetwork, saltNetwork, sourceNetwork } from "./network.js";

const NETWORK_HEADER = "X-Bonjou-Network";
const RATE_WINDOW_MS = 60_000;
const MAX_FRAME_BYTES = 128 * 1024;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

function originAllowed(origin, configured) {
  if (!origin || typeof configured !== "string") return false;
  return configured.split(",").map((value) => value.trim()).some((value) => {
    try {
      const url = new URL(value);
      return ["https:", "http:"].includes(url.protocol) && url.origin === value && value === origin;
    } catch {
      return false;
    }
  });
}

function response(status, body, origin) {
  const headers = new Headers({ "Cache-Control": "no-store" });
  if (origin) {
    headers.set("Access-Control-Allow-Origin", origin);
    headers.set("Access-Control-Allow-Methods", "GET, OPTIONS");
    headers.set("Vary", "Origin");
  }
  return new Response(body, { status, headers });
}

export default {
  async fetch(request, env) {
    const { pathname } = new URL(request.url);
    if (pathname !== "/healthz" && pathname !== "/ws") return response(404, "Not found");
    const origin = request.headers.get("Origin");
    const allowed = originAllowed(origin, env.ALLOWED_ORIGINS);
    if (request.method === "OPTIONS") return response(allowed ? 204 : 403, null, allowed ? origin : null);
    if (request.method !== "GET") return response(405, "Method not allowed");
    const stub = env.COORDINATOR_HUB.getByName("bonjou-hub-v1");
    if (pathname === "/healthz") {
      const health = await stub.fetch(new Request("https://coordinator.internal/healthz"));
      const headers = new Headers(health.headers);
      if (allowed) {
        headers.set("Access-Control-Allow-Origin", origin);
        headers.set("Vary", "Origin");
      }
      return new Response(health.body, { status: health.status, headers });
    }
    if (!allowed) return response(403, "Origin not allowed");
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") return response(426, "WebSocket upgrade required", origin);
    const source = sourceNetwork(request.headers);
    if (!source) return response(503, "Trusted client address unavailable", origin);
    // Construct a fresh private request. No browser-supplied identity or proxy
    // header crosses the binding boundary, and the raw address is never stored.
    return stub.fetch(new Request("https://coordinator.internal/ws", {
      headers: { Upgrade: "websocket", [NETWORK_HEADER]: await hashNetwork(source) },
    }));
  },
};

export class BonjouCoordinatorHub extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.hub = new Coordinator({
      now: Date.now,
      randomBytes: (length) => crypto.getRandomValues(new Uint8Array(length)),
    });
    this.sockets = new Map();
    this.sql = ctx.storage.sql;
    this.sql.exec("CREATE TABLE IF NOT EXISTS routing_salt (id INTEGER PRIMARY KEY CHECK(id = 1), value TEXT NOT NULL)");
    let salt = this.sql.exec("SELECT value FROM routing_salt WHERE id = 1").toArray()[0]?.value;
    if (salt === undefined) {
      salt = [...crypto.getRandomValues(new Uint8Array(32))].map((byte) => byte.toString(16).padStart(2, "0")).join("");
      this.sql.exec("INSERT INTO routing_salt (id, value) VALUES (1, ?)", salt);
    }
    if (typeof salt !== "string" || !/^[a-f0-9]{64}$/.test(salt)) throw new TypeError("Invalid coordinator routing salt");
    this.routingSalt = Uint8Array.from(salt.match(/../g), (byte) => Number.parseInt(byte, 16));
    this.sql.exec("CREATE TABLE IF NOT EXISTS creation_rates (network TEXT PRIMARY KEY, start INTEGER NOT NULL, count INTEGER NOT NULL)");
    this.sql.exec("DELETE FROM creation_rates WHERE start <= ?", Date.now() - RATE_WINDOW_MS);
    this.hub.restoreRates(this.sql.exec("SELECT network, start, count FROM creation_rates").toArray());
    for (const socket of ctx.getWebSockets()) {
      if (socket.readyState !== WebSocket.OPEN) continue;
      try {
        const attachment = socket.deserializeAttachment();
        this.hub.restorePeer(attachment);
        this.sockets.set(attachment.id, socket);
      } catch {
        socket.close(1008, "Invalid coordinator session");
      }
    }
    // A disconnect can be the event that wakes this object, after the closed
    // socket has disappeared from getWebSockets(). Refresh survivors' own
    // rosters after every OPEN attachment is restored to remove stale peers.
    for (const [id] of this.sockets) {
      const peer = this.hub.exportPeer(id);
      if (peer.lobby || peer.code) this.deliver(this.hub.message(id, { type: "hello" }));
    }
  }

  async fetch(request) {
    const { pathname } = new URL(request.url);
    if (request.method === "GET" && pathname === "/healthz")
      return Response.json({ status: "ok", rooms: this.hub.roomCount }, { headers: { "Cache-Control": "no-store" } });
    if (request.method !== "GET" || pathname !== "/ws") return response(404, "Not found");
    const provisional = request.headers.get(NETWORK_HEADER);
    if (!provisional || !/^[a-f0-9]{64}$/.test(provisional)) return response(503, "Trusted network unavailable");
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") return response(426, "WebSocket upgrade required");
    // The provisional ingress digest is transient. Private persisted salt keeps
    // attachments and short rate windows from supporting address enumeration.
    const network = await saltNetwork(provisional, this.routingSalt);
    const [client, socket] = Object.values(new WebSocketPair());
    let id;
    do {
      id = [...crypto.getRandomValues(new Uint8Array(8))].map((byte) => byte.toString(16).padStart(2, "0")).join("");
    } while (this.sockets.has(id));
    const deliveries = this.hub.addPeer({ id, network });
    this.ctx.acceptWebSocket(socket);
    this.sockets.set(id, socket);
    socket.serializeAttachment(this.hub.exportPeer(id));
    this.deliver(deliveries);
    return new Response(null, { status: 101, webSocket: client });
  }

  webSocketMessage(socket, message) {
    const attachment = socket.deserializeAttachment();
    if (!attachment || this.sockets.get(attachment.id) !== socket || socket.readyState !== WebSocket.OPEN) {
      this.retire(socket, 1008, "Invalid coordinator session");
      return;
    }
    // Reject obviously oversized text before allocating an encoded copy. UTF-8
    // uses at least one byte per UTF-16 code unit; the precise byte bound below
    // still catches smaller strings containing multibyte characters.
    if (typeof message === "string" && message.length > MAX_FRAME_BYTES) {
      this.retire(socket, 1009, "Control frame too large");
      return;
    }
    const bytes = typeof message === "string" ? encoder.encode(message)
      : message instanceof ArrayBuffer ? new Uint8Array(message) : null;
    if (!bytes) {
      this.retire(socket, 1007, "Invalid control frame");
      return;
    }
    if (bytes.byteLength > MAX_FRAME_BYTES) {
      this.retire(socket, 1009, "Control frame too large");
      return;
    }
    let frame;
    let deliveries;
    try {
      frame = JSON.parse(typeof message === "string" ? message : decoder.decode(bytes));
      deliveries = this.hub.message(attachment.id, frame);
    } catch {
      this.retire(socket, 1007, "Invalid control frame");
      return;
    }
    socket.serializeAttachment(this.hub.exportPeer(attachment.id));
    // Go matches JSON field names case-insensitively. Rate persistence follows
    // its last matching type value without persisting the message itself.
    let type;
    for (const [key, value] of Object.entries(frame ?? {})) if (key.toLowerCase() === "type" && value !== null) type = value;
    if (type === "create") this.persistRate(attachment.network);
    this.deliver(deliveries);
  }

  webSocketClose(socket) {
    this.remove(socket);
    if (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CLOSING)
      socket.close(1000, "Session ended");
  }

  webSocketError(socket) {
    this.retire(socket, 1011, "Coordinator connection failed");
  }

  remove(socket) {
    const attachment = socket.deserializeAttachment();
    if (!attachment || this.sockets.get(attachment.id) !== socket) return;
    this.sockets.delete(attachment.id);
    this.deliver(this.hub.removePeer(attachment.id));
  }

  retire(socket, code, reason) {
    this.remove(socket);
    if (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CLOSING) socket.close(code, reason);
  }

  deliver(deliveries) {
    const failed = new Set();
    for (const { id, frame } of deliveries) {
      const socket = this.sockets.get(id);
      if (!socket || failed.has(socket)) continue;
      try {
        if (socket.readyState !== WebSocket.OPEN) throw new Error("Socket unavailable");
        socket.send(JSON.stringify(frame));
      } catch {
        failed.add(socket);
      }
    }
    for (const socket of failed) this.retire(socket, 1011, "Coordinator connection failed");
  }

  persistRate(network) {
    const now = Date.now();
    const rate = this.hub.exportRates().find((entry) => entry.network === network);
    this.sql.exec("DELETE FROM creation_rates WHERE start <= ?", now - RATE_WINDOW_MS);
    if (rate) this.sql.exec(
      "INSERT INTO creation_rates (network, start, count) VALUES (?, ?, ?) ON CONFLICT(network) DO UPDATE SET start = excluded.start, count = excluded.count",
      rate.network, rate.start, rate.count,
    );
    this.scheduleRateExpiry();
  }

  scheduleRateExpiry() {
    const { expires } = this.sql.exec("SELECT MIN(start) + ? AS expires FROM creation_rates", RATE_WINDOW_MS).one();
    if (expires !== null) this.ctx.waitUntil(this.ctx.storage.setAlarm(Math.max(Date.now() + 1, expires)));
    else this.ctx.waitUntil(this.ctx.storage.deleteAlarm());
  }

  alarm() {
    this.sql.exec("DELETE FROM creation_rates WHERE start <= ?", Date.now() - RATE_WINDOW_MS);
    this.hub.restoreRates(this.sql.exec("SELECT network, start, count FROM creation_rates").toArray());
    this.scheduleRateExpiry();
  }
}
