# Contributing

Use the Go version declared in `go.mod` and read `AGENTS.md` before changing
code. Keep application logic under `internal/` and entry-point wiring under
`cmd/`. Preserve behavior on macOS, Windows, and Linux.

Before submitting a focused pull request, run:

```sh
gofmt -w <changed-go-files>
go test ./...
golangci-lint run ./...
go build ./cmd/bonjou ./cmd/bonjou-relay
```

For protocol changes, regenerate `internal/network/testdata/protocol-v2.json`
with `BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors`.
Coordinate the browser implementation and provenance update in
[bonjou-web](https://github.com/bonjou-app/bonjou-web), and update the pinned
browser revision in `.github/workflows/ci.yml`. The compatibility job tests that
browser revision with the candidate Go vectors and the Go relay. Each product
still builds without a sibling checkout.

Keep real credentials, `.env` files, user state, and payloads out of source and
test fixtures. Include the behavior change and verification results in your
pull request. Use [SECURITY.md](SECURITY.md) for vulnerability reports.
