#!/bin/sh
set -eu

port=${PORT:-10000}
case "$port" in
    ''|*[!0-9]*)
        echo 'PORT must be an integer between 1 and 65535' >&2
        exit 1
        ;;
esac
if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    echo 'PORT must be an integer between 1 and 65535' >&2
    exit 1
fi

trust_proxy=${BONJOU_RELAY_TRUST_PROXY:-false}
case "$trust_proxy" in
    true|false) ;;
    *)
        echo 'BONJOU_RELAY_TRUST_PROXY must be true or false' >&2
        exit 1
        ;;
esac

set -- \
    -addr "0.0.0.0:$port" \
    -origins "${BONJOU_RELAY_ORIGINS:-https://bonjou.vercel.app}" \
    -trust-proxy="$trust_proxy" \
    -log-dir /var/log/bonjou-relay

if [ -n "${BONJOU_RELAY_CLIENT_IP_HEADER:-}" ]; then
    set -- "$@" -client-ip-header "$BONJOU_RELAY_CLIENT_IP_HEADER"
fi

exec /usr/local/bin/bonjou-relay "$@"
