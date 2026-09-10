# Bonjou Repository Guidelines

This is the canonical project-wide guidance for AI agents. `CLAUDE.md` imports
this file; keep shared instructions here instead of duplicating them across
agent-specific files. Where the legacy `.rules` file conflicts with this file,
follow this file. Read toolchain versions and scripts from the current manifests.

## Default Working Practices

- Treat relevant skills, sound engineering practices, security, and verification as defaults. The user should not need to repeat "use the best skills and guidelines" in each task.
- Before editing, inspect the working tree, applicable instructions, affected code, and relevant project docs. Preserve unrelated user changes. Check assumptions against the implementation.
- Select the smallest useful set of available skills and read their instructions. Use architecture/design guidance for structural work, security guidance for trust boundaries and secrets, frontend/accessibility guidance for UI work, and testing/deployment guidance for those tasks. Respect each skill's scope; do not load unrelated skills or introduce a new framework just because a skill mentions it.
- For substantial design changes, explain the problem, realistic options, recommendation, and compatibility impact before implementation. Routine fixes and documentation updates should stay proportional to the task.
- Carry authorized work through implementation and appropriate verification. Make routine, reversible decisions within scope; ask only for missing information that changes the outcome or authorization that has not been supplied. Do not ask again for approval already given. Prepare a concrete migration and its validation before requesting any remaining approval for repository transfers, visibility changes, or history rewrites.
- Use current primary documentation for platform behavior and security recommendations that may have changed. Distinguish verified facts, inferences, and proposals.
- Review touched code for security, error handling, maintainability, and platform impact. Keep changes focused; avoid speculative abstractions, unrelated refactors, and silent dependency upgrades.
- Match verification to the change: meaningful regression tests for behavior changes; both Go and browser protocol suites for protocol work; browser verification for UI changes. For documentation-only changes, check accuracy, links, and the diff without adding artificial tests. Report checks actually run and any gaps.
- Finish with the outcome, relevant files, verification results, and remaining decisions or limitations. State security findings with evidence and redact secret values. A clean scan does not prove that a repository contains no secrets.

## Product and Repository Boundaries

- Bonjou is an open-source project. Keep product source, protocol documentation, synthetic test vectors, and reusable self-hosting templates suitable for public distribution. A private repository is not a substitute for secret management.
- This repository contains the Go CLI, Go relay, and canonical protocol vectors. The marketing website and browser app live in the sibling `bonjou-app/bonjou-web` repository. Each product builds independently; no parent repository or submodule is required.
- The approved organization architecture and migration state are documented in [docs/repository-architecture.md](docs/repository-architecture.md). Update repository links and documentation together when ownership changes; preserve history, license notices, releases, and working installation channels.
- The Go relay has its own deployment but stays in this Go module. It must forward opaque content without client encryption keys, decryption, or payload storage.
- Go owns `internal/network/testdata/protocol-v2.json`. The web repo keeps a copy pinned to a reviewed CLI commit with a checksum. The browser compatibility job checks a pinned web revision against this repository's candidate vectors and relay. Coordinate changes through linked PRs and explicit revision updates.
- An organization profile is the project overview. Agent instructions belong in each product repository so independent clones have the guidance they need.

## Build, Test, and Development Commands

- `go run ./cmd/bonjou` — run the CLI locally
- `go run ./cmd/bonjou-relay` — run the web relay locally (listens on `127.0.0.1:46330`)
- `go test ./...` — full test suite; `go test ./internal/network -run TestName` for a single test
- `./scripts/deploy-relay.sh` — cross-compile the relay and install it on the server
- Browser checks run in `bonjou-web`: `npm run check:protocol`, `npm test`, and `npm run build`. Run both implementations for protocol changes.
- `gofmt -w <file>` — required before committing, no exceptions
- `golangci-lint run ./...` — lint the module (config in `.golangci.yml`)
- `./scripts/build.sh` — cross-compile Linux/macOS/Windows binaries into `dist/bin/`
- `./scripts/package.sh` — build release artifacts + package metadata under `dist/`

## Code Style & Naming Conventions

- **Receiver names** are fixed per type — match existing methods (`t` `*TransferService`, `d` `*DiscoveryService`, `s` `*Session`, `h` `*Handler`, `m` `*Manager`, `l` `*Logger`, `c` `*Config`). In `internal/relay`: `r` `*Room`, `h` `*Hub`, `p` `*Peer`, `c` `*Conn`, `v` `*Rendezvous`, `x` `*transfer`, `s` `*Server`.
- **Sentinel errors**: unexported `errCamelCase`, exported `ErrPascalCase`. Handler command methods: `cmd` + PascalCase (`cmdSend`, `cmdFile`).
- **Error wrapping**: Use `fmt.Errorf("lowercase context: %w", err)` — no trailing punctuation.
- **File permissions**: Use `0o`-prefixed octal (`0o755`, `0o644`, `0o600`).
- **Imports**: Grouped stdlib → third-party → internal, blank line between groups.
- **Forbidden**: stdlib `log` (use `internal/logger`), `panic`, `math/rand` for security values (use `crypto/rand`), or third-party test frameworks (stdlib `testing` only).
- **Platform-specific code**: Provide both a `_windows.go` and a `_other.go`/`_unix.go` file with matching `//go:build` constraints.

## Architecture Rules

- All Go code lives under `cmd/` and `internal/`. There are exactly two binaries — `cmd/bonjou` (the CLI) and `cmd/bonjou-relay` (the web relay). Do not introduce new application roots without a concrete architectural need; `.github/` holds CI and contributor configuration.
- **The relay is a dumb pipe.** `internal/relay` must never import `internal/network`, hold key material, or decrypt anything. It routes on a destination peer id and forwards opaque payloads. If a change would give the relay the ability to read user content, the change is wrong.
- **Two implementations of protocol v2.** Go lives in `internal/network`; the browser implementation is `src/share/crypto.ts` in `bonjou-web`. Regenerate canonical vectors with `BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors`, coordinate both implementations, and update the browser fixture provenance and CI revision pins. Do not hand-edit generated vectors or skip a missing fixture.
- `internal/` packages never write to `os.Stdout` — user-facing output goes through `ui.UI` or `Result.Output`.
- **New `@commands`**: register it in the `Handle()` switch in `internal/commands/handler.go` and `helpText()` in `internal/commands/help.go`.
- The version lives in two places that must stay in sync: `internal/version/version.go` and the `VERSION` file. Only change them when cutting a release.
- **Wire protocol**: Ask/evaluate carefully before changing `envelope` / `sealedEnvelope` in `transfer.go`, AEAD framing in `crypto.go`, or the TOFU pin format in `known_peers.go`.

## Security Invariants

- **Metadata-first approval**: never write an incoming file/folder payload before the user explicitly approves it.
- **Sign/Verify**: Do not bypass `signEnvelope` / `verifyEnvelope`.
- **Sanitize paths**: Sanitize peer-supplied paths with `uniquePath` / `UniquePath` before writing under `~/.bonjou/received/`.
- **Secrets**: `config.json` is written `0o600`; never log the `Config.Secret` field. Load/persist secrets through `internal/config/secretstore.go`.
- **Relay nginx config**: `proxy_request_buffering off` is mandatory. Without it nginx spools every upload to disk before forwarding, which silently turns a relay that stores nothing into one that writes every file to `/var/lib/nginx`.

## Open-Source Publication and Secrets

- Classify values by purpose and provenance. Dependency integrity hashes, release checksums, protocol labels, and public-key fingerprints are normally public. Password hashes, bearer tokens, and derived session keys remain sensitive even when represented as hashes or hex strings. Avoid publishing user-linked metadata without a reason.
- The fixed private keys and derived values in `internal/network/vectors_test.go` and `internal/network/testdata/protocol-v2.json` are intentional public test fixtures. Keep them synthetic, confined to tests, and separate from runtime identity generation. Never replace fixtures with real user keys.
- Treat values embedded in browser JS, HTML, public assets, source maps, and `VITE_*` configuration as public. Keep service credentials in deployment secret stores or server-side environment variables. End-to-end encryption keys belong on the participating clients; never move them to the relay to conceal them from source code.
- Keep real credentials, private key files, local `.env` files, `~/.bonjou/` state, received payloads, and user logs out of Git, fixtures, screenshots, and build artifacts. Maintain ignore rules for local secrets and use placeholder-only examples. Private operations repositories must also keep credentials outside Git.
- Before publishing new code or extracting a repository, review tracked files, relevant Git history, and artifacts for secrets. Use secret scanning and push protection where available; review findings without printing values. Keep exceptions narrow and document synthetic fixtures rather than excluding all crypto code.
- If a real credential was committed, treat it as exposed: revoke or rotate it, remove the source of exposure, and coordinate any necessary history cleanup. Deleting a file or making a repository private does not erase existing clones or public forks.
- Preserve the existing license and attribution when moving source. `"private": true` in the web app’s `package.json` prevents npm publication; it does not control GitHub visibility or the source license.

## Links

- [CLAUDE.md](CLAUDE.md)
- [Web design](https://github.com/bonjou-app/bonjou-web/blob/main/DESIGN.md)
- [PRODUCT.md](PRODUCT.md)
- [Repository architecture review](docs/repository-architecture.md)
- [Security model](docs/security-model.md)
- [Contributing](CONTRIBUTING.md)
- [Reporting vulnerabilities](SECURITY.md)
