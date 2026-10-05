#!/usr/bin/env bash
# Build the Bonjou Web coordinator for the server's architecture and install it.
#
# The coordinator is a single static binary. Nothing is compiled on the server:
# it has no Go toolchain and does not need one.
#
# Idempotent — safe to re-run to deploy a new build.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

SSH_HOST="${BONJOU_RELAY_SSH_HOST:?Set BONJOU_RELAY_SSH_HOST to a verified SSH host or alias}"
RELAY_HOST="${BONJOU_RELAY_HOSTNAME:?Set BONJOU_RELAY_HOSTNAME to the replacement coordinator hostname}"
RELAY_ORIGINS="${BONJOU_RELAY_ORIGINS:-https://bonjou.vercel.app}"
GOARCH_TARGET="${BONJOU_RELAY_GOARCH:-arm64}"

BINARY="$ROOT_DIR/dist/bin/bonjou-relay-linux-$GOARCH_TARGET"

echo "==> Building coordinator for linux/$GOARCH_TARGET"
mkdir -p "$ROOT_DIR/dist/bin"
(cd "$ROOT_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_TARGET" \
    go build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/bonjou-relay)

echo "==> Uploading to $SSH_HOST"
scp -q "$BINARY" "$SSH_HOST:/tmp/bonjou-relay"
scp -q "$ROOT_DIR/packaging/relay/bonjou-relay.service" "$SSH_HOST:/tmp/bonjou-relay.service"
scp -q "$ROOT_DIR/packaging/relay/nginx-bonjou-relay.conf" "$SSH_HOST:/tmp/nginx-bonjou-relay.conf"

echo "==> Installing on $SSH_HOST"
ssh "$SSH_HOST" \
    RELAY_HOST="$RELAY_HOST" \
    RELAY_ORIGINS="$RELAY_ORIGINS" \
    'bash -s' <<'REMOTE'
set -euo pipefail

# Dedicated unprivileged account; the coordinator never needs a home or a shell.
if ! id bonjou-relay >/dev/null 2>&1; then
    sudo useradd --system --no-create-home --shell /usr/sbin/nologin bonjou-relay
fi

sudo install -d -o bonjou-relay -g bonjou-relay -m 0750 /var/log/bonjou-relay
sudo install -m 0755 /tmp/bonjou-relay /usr/local/bin/bonjou-relay
sudo install -m 0644 /tmp/bonjou-relay.service /etc/systemd/system/bonjou-relay.service

printf 'BONJOU_RELAY_ORIGINS=%s\n' "$RELAY_ORIGINS" | sudo tee /etc/default/bonjou-relay >/dev/null
sudo chmod 0644 /etc/default/bonjou-relay

sudo systemctl daemon-reload
sudo systemctl enable --now bonjou-relay
sudo systemctl restart bonjou-relay

sudo install -d /var/www/html

if ! sudo test -d "/etc/letsencrypt/live/${RELAY_HOST}"; then
    echo "==> Requesting a certificate for ${RELAY_HOST}"
    # The full vhost cannot load yet — its TLS directives reference a
    # certificate that does not exist. Serve only the ACME challenge for
    # this hostname meanwhile. Without a vhost claiming the name, port 80
    # falls through to whichever default server is configured, which on
    # this box answers 404 and fails the challenge.
    sudo tee /etc/nginx/sites-available/bonjou-relay >/dev/null <<ACME
server {
    listen 80;
    listen [::]:80;
    server_name ${RELAY_HOST};
    location /.well-known/acme-challenge/ { root /var/www/html; }
    location / { return 404; }
}
ACME
    sudo ln -sf /etc/nginx/sites-available/bonjou-relay /etc/nginx/sites-enabled/bonjou-relay
    sudo nginx -t
    sudo systemctl reload nginx

    sudo certbot certonly --webroot -w /var/www/html \
        -d "${RELAY_HOST}" --non-interactive --agree-tos \
        --register-unsafely-without-email
fi

# Now the real vhost, validated before any reload so a mistake here cannot
# take down anything else on this box.
sudo sed "s/RELAY_HOSTNAME/${RELAY_HOST}/g" /tmp/nginx-bonjou-relay.conf \
    | sudo tee /etc/nginx/sites-available/bonjou-relay >/dev/null
sudo ln -sf /etc/nginx/sites-available/bonjou-relay /etc/nginx/sites-enabled/bonjou-relay

# Point the vhost at the certificate if certbot has not already edited it.
if ! grep -q ssl_certificate /etc/nginx/sites-available/bonjou-relay; then
    sudo sed -i "/listen \[::\]:443 ssl;/a\\
    ssl_certificate /etc/letsencrypt/live/${RELAY_HOST}/fullchain.pem;\\
    ssl_certificate_key /etc/letsencrypt/live/${RELAY_HOST}/privkey.pem;\\
    include /etc/letsencrypt/options-ssl-nginx.conf;\\
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;" \
        /etc/nginx/sites-available/bonjou-relay
fi

sudo nginx -t
sudo systemctl reload nginx

echo "==> systemd status"
sudo systemctl --no-pager --lines=5 status bonjou-relay || true
REMOTE

echo "==> Verifying"
curl -fsS "https://${RELAY_HOST}/healthz" && echo
echo "Coordinator live at https://${RELAY_HOST}"
