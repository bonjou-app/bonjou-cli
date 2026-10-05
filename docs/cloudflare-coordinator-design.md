# Cloudflare coordinator recovery

The production Oracle coordinator was deleted. Render and Northflank require
card verification for this account; the user has explicitly rejected adding a
card and approved a Cloudflare-compatible coordinator on the Free plan.

## Architecture

Keep the two Go binaries and existing Docker/VPS coordinator. Add a JavaScript
Cloudflare platform adapter under `packaging/relay/cloudflare/`, using one
hibernating SQLite-backed Durable Object as the room-code authority. Separate
hashed source-network buckets preserve lobby isolation, globally unique room
codes, and the distinction between missing rooms and foreign-network rooms.

The outer Worker validates the exact website Origin and WebSocket upgrade. It
uses only Cloudflare's edge-provided connecting IP, rejecting Worker subrequests,
invalid addresses, and the cross-zone Worker sentinel. Forwarding headers
supplied by callers cannot select a network. IPv4 groups by exact address;
IPv4-mapped IPv6 normalizes to IPv4; native IPv6 groups by /64, matching Go.

The hub accepts only existing hello, create, join, and opaque signaling control
messages. It never receives profiles, chat, file metadata, or file payloads.
Application data remains authenticated, encrypted, direct WebRTC. Existing
browser and Go protocol-v2 crypto and known-answer fixtures remain unchanged.

## State and lifecycle

WebSocket attachments contain only versioned peer ID, public session key, hashed
network identity, and current room membership. Rebuild the hub from live attached
sockets after hibernation. Retire rooms when their last peer departs. Preserve
creation-rate windows across hibernation using SQLite metadata; never persist
signaling ciphertext or application content. Avoid recurring timers.

Preserve canonical Go frame sizes, room capacities, code alphabet and format,
key immutability, membership narrowing, and error codes. Cloudflare's synchronous
WebSocket send API cannot reproduce Go's observable send queue and write deadline;
document and test that platform boundary instead of claiming identical transport.

## Validation and rollout

Use Node's built-in test runner for protocol, capacities, network normalization,
metadata restoration, rate windows, and routing. Exercise the adapter in the
Cloudflare-compatible runtime, including hibernation where supported. Run the
existing coordinator protocol smoke and real browser room/QR/chat/approved file
integrity flows against it. Verify live ingress spoof resistance and separate
source-network room rejection before changing the website's coordinator URL.

Build and test both repositories, validate a web preview, merge the exact tested
heads, and verify the real production deployment and source revision. Stay on
Cloudflare Free; daily quota exhaustion may affect availability, but no paid
upgrade, card, or paid overage is authorized.

## Implementation plan

1. Implement a pure protocol hub and Node tests, independent of WebSockets.
2. Add the Worker ingress, network normalization, hibernating Durable Object,
   versioned attachment restoration, and pinned deployment tooling.
3. Add runtime tests and CI smoke/browser coverage, then review security bounds.
4. Deploy using the user's authenticated Cloudflare account on Free, prove
   ingress/network boundaries, and update the web build to the actual URL.
5. Run repository CI and live production sharing tests before final handoff.
