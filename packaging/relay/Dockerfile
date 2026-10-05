# Build from the module's reviewed toolchain version. No Go toolchain is needed
# on the runtime host, and only the signaling coordinator ships in this image.
FROM golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/bonjou-relay ./cmd/bonjou-relay

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659
RUN addgroup -S -g 10001 bonjou \
    && adduser -S -D -H -u 10001 -G bonjou bonjou \
    && mkdir -p /var/log/bonjou-relay \
    && chown bonjou:bonjou /var/log/bonjou-relay

COPY --from=build /out/bonjou-relay /usr/local/bin/bonjou-relay
COPY --chmod=0555 packaging/relay/start-container.sh /usr/local/bin/bonjou-relay-start
COPY LICENSE /usr/share/licenses/bonjou/LICENSE

ENV PORT=10000 \
    BONJOU_RELAY_ORIGINS=https://bonjou.vercel.app \
    BONJOU_RELAY_TRUST_PROXY=false
USER 10001:10001
EXPOSE 10000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -T 2 -O /dev/null "http://127.0.0.1:${PORT}/healthz" || exit 1
ENTRYPOINT ["/usr/local/bin/bonjou-relay-start"]
