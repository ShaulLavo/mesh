#!/usr/bin/env bash
# A persisted origin service must return after a daemon crash. If its directory
# later disappears, the daemon stays alive and reports the service unavailable.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
ROOT="$T/site"
DAEMON=""

cleanup() {
  [ -z "$DAEMON" ] || kill -9 "$DAEMON" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

wait_for_socket() {
  local pid=$1
  for _ in $(seq 100); do
    kill -0 "$pid" 2>/dev/null || return 1
    [ -S "$MESH_STATE_DIR/daemon.sock" ] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_site() {
  local pid=$1
  for _ in $(seq 100); do
    kill -0 "$pid" 2>/dev/null || return 1
    body=$(curl --fail --silent --max-time 0.2 "http://127.0.0.1:$PORT/site/" 2>/dev/null) &&
      [ "$body" = "SERVICE_RESTART_MARKER" ] && return 0
    sleep 0.05
  done
  return 1
}

mkdir -p "$ROOT" "$T/bin"
printf SERVICE_RESTART_MARKER >"$ROOT/index.html"
cat >"$T/bin/tailscale" <<'TAILSCALE'
#!/usr/bin/env sh
printf '%s\n' '{"BackendState":"Running","Self":{"HostName":"test","TailscaleIPs":["127.0.0.1"]}}'
TAILSCALE
chmod +x "$T/bin/tailscale"
PORT=$(python3 - "$$" <<'PY'
import socket, sys

# Own band per concurrent script; bind-then-close alone hands the port back
# before the daemon claims it.
base = 20000 + (int(sys.argv[1]) % 900) * 16
for offset in range(16 * 900):
    port = 20000 + (base - 20000 + offset) % (16 * 900)
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        listener.bind(("127.0.0.1", port))
    except OSError:
        listener.close()
        continue
    print(port)
    listener.close()
    break
else:
    raise SystemExit("no free port in this band")
PY
)

# Create the service through the same daemon control request T14 will call.
"$MESH" daemon >"$T/seed.log" 2>&1 &
DAEMON=$!
wait_for_socket "$DAEMON" || fail "seed daemon did not start: $(cat "$T/seed.log")"
python3 - "$MESH_STATE_DIR/daemon.sock" "$ROOT" <<'PY'
import json
import socket
import struct
import sys

request = json.dumps({
    "type": "service.upsert",
    "requestId": "integration-upsert",
    "service": {"name": "site", "kind": "static", "target": sys.argv[2]},
}).encode()

def receive(connection, length):
    payload = b""
    while len(payload) < length:
        chunk = connection.recv(length - len(payload))
        if not chunk:
            raise SystemExit("daemon closed an incomplete response")
        payload += chunk
    return payload

with socket.socket(socket.AF_UNIX) as connection:
    connection.connect(sys.argv[1])
    connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
    header = receive(connection, 5)
    if len(header) != 5 or header[0] != 1:
        raise SystemExit(f"invalid response header: {header!r}")
    length = struct.unpack(">I", header[1:])[0]
    payload = receive(connection, length)
response = json.loads(payload)
if response.get("type") != "service.upserted" or not response.get("service", {}).get("healthy"):
    raise SystemExit(f"service upsert failed: {response!r}")
PY
kill "$DAEMON"
wait "$DAEMON" || fail "seed daemon did not stop cleanly"
DAEMON=""

PATH="$T/bin:$PATH" "$MESH" daemon --tailnet-port "$PORT" >"$T/daemon1.log" 2>&1 &
DAEMON=$!
wait_for_site "$DAEMON" || fail "first daemon did not serve the site: $(cat "$T/daemon1.log")"
kill -9 "$DAEMON"
wait "$DAEMON" 2>/dev/null
DAEMON=""

PATH="$T/bin:$PATH" "$MESH" daemon --tailnet-port "$PORT" >"$T/daemon2.log" 2>&1 &
DAEMON=$!
wait_for_site "$DAEMON" || fail "restarted daemon did not restore the site: $(cat "$T/daemon2.log")"

mv "$ROOT" "$T/removed-site"
STATUS=$(curl --silent --output "$T/missing.out" --write-out '%{http_code}' --max-time 1 "http://127.0.0.1:$PORT/site/")
[ "$STATUS" = 503 ] || fail "missing service root returned $STATUS, want 503: $(cat "$T/missing.out")"
kill -0 "$DAEMON" 2>/dev/null || fail "missing service root stopped the daemon"

echo "PASS: persisted service returned after daemon restart and a missing root stayed contained"
