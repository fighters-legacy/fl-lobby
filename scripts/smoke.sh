#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 MKZ Systems LLC
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Loopback smoke: build the real binary, run it, and drive the three endpoints over a real socket.
# The unit suite exercises the handler in-process, so what this adds is main.go's wiring -- a flag
# that never reaches the config, a listener that never starts, a shutdown that hangs.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${SMOKE_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
BIN="$(mktemp -d)/fl-lobby"

cleanup() {
    if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }

echo "==> building"
go build -o "$BIN" ./cmd/fl-lobby

echo "==> starting on :${PORT}"
# A 5s heartbeat keeps the expiry check below fast (5 x 2.5 = 12.5s TTL) without a magic sleep
# against the 75s default.
"$BIN" -addr "127.0.0.1:${PORT}" -heartbeat 5s -log-level debug &
PID=$!

for _ in $(seq 1 50); do
    if curl -fsS "${BASE}/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.1
done
curl -fsS "${BASE}/healthz" >/dev/null || fail "service never became healthy"

echo "==> empty list is an array"
[[ "$(curl -fsS "${BASE}/v1/servers")" == "[]" ]] || fail "empty list was not []"

echo "==> register (expect 201)"
BODY='{"name":"Smoke Server","port":4778,"players":3,"max_players":16,"mode":"builtin:tdm","mission":"fjord","visibility":"public"}'
code=$(curl -fsS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$BODY" "${BASE}/v1/servers")
[[ "$code" == "201" ]] || fail "first registration returned $code, want 201"

echo "==> heartbeat (expect 200)"
code=$(curl -fsS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$BODY" "${BASE}/v1/servers")
[[ "$code" == "200" ]] || fail "heartbeat returned $code, want 200"

echo "==> the entry is listed, at the observed loopback address"
list=$(curl -fsS "${BASE}/v1/servers")
echo "$list" | grep -q '"host":"127.0.0.1"' || fail "listed host is not the observed address: $list"
echo "$list" | grep -q '"port":4778'         || fail "advertised port missing: $list"
echo "$list" | grep -q '"name":"Smoke Server"' || fail "name missing: $list"
[[ $(echo "$list" | grep -o '"host"' | wc -l) == "1" ]] || fail "heartbeat duplicated the entry: $list"

echo "==> deregister (expect 204), entry gone"
code=$(curl -fsS -o /dev/null -w '%{http_code}' -X DELETE -H 'Content-Type: application/json' -d '{"port":4778}' "${BASE}/v1/servers")
[[ "$code" == "204" ]] || fail "deregistration returned $code, want 204"
[[ "$(curl -fsS "${BASE}/v1/servers")" == "[]" ]] || fail "entry survived deregistration"

echo "==> an entry expires when heartbeats stop"
curl -fsS -o /dev/null -X POST -H 'Content-Type: application/json' -d "$BODY" "${BASE}/v1/servers"
sleep 14   # TTL is 5s x 2.5 = 12.5s
[[ "$(curl -fsS "${BASE}/v1/servers")" == "[]" ]] || fail "entry outlived its TTL"

echo "==> a private server is refused"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d '{"name":"x","port":4779,"visibility":"private"}' "${BASE}/v1/servers")
[[ "$code" == "400" ]] || fail "private registration returned $code, want 400"

echo "==> shuts down cleanly on SIGTERM"
kill -TERM "$PID"
for _ in $(seq 1 50); do
    if ! kill -0 "$PID" 2>/dev/null; then break; fi
    sleep 0.1
done
kill -0 "$PID" 2>/dev/null && fail "did not exit within 5s of SIGTERM"
PID=""

echo "SMOKE OK"
