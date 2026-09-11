# Bonjou Web coordinator integration

This change pairs with `bonjou-app/bonjou-web` commit
`59e2f8a7fd3ad17697075889693c89bcf08b1521` on `codex/bonjou-web-revamp`.
The browser compatibility workflow is pinned to that exact web revision.

The coordinator groups source-network candidates, scopes rooms to that
network, and forwards opaque encrypted WebRTC signaling. Profiles, chat,
file metadata, and file payloads travel directly between browsers. Legacy
application frames and HTTP payload endpoints are rejected. The nginx
configuration no longer forwards `/t/`. The historical `bonjou-relay` binary
name, service name, and deployment environment names remain intact.

This is a coordinated web/control-plane update, not a backward-compatible
replacement for the payload relay. Deploy the coordinator and matching web
revision together. No production service or deployment was changed while
preparing these commits. The Go CLI's transfer protocol, dependencies,
canonical cryptographic vectors, and upstream test repairs are preserved.

Validation against the standalone web app: `go test ./...`,
`golangci-lint run ./...`, canonical vector comparison, 64 browser tests,
web build, coordinator smoke, responsive UI, direct discovery/chat/file
transfers, room and session workflows, motion controls, and 16 accessibility
surfaces all pass locally. Tests use separate browser processes on one Mac;
physical mobile devices and arbitrary networks remain outside this check.
