#!/usr/bin/env bash

private_app_fixture() {
  python3 "$REPO_ROOT/integration/helpers/private_app_fixture.py" "$@"
}

app_curl() {
  curl --noproxy '*' --silent --show-error --max-time 3 \
    --haproxy-clientip 100.64.0.9 --cacert "$TEST_ROOT/tls.pem" \
    --resolve "$APP_ID.mesh.test:$PROXY_PORT:127.0.0.1" "$@"
}

app_endpoint() {
  printf 'https://%s.mesh.test:%s' "$APP_ID" "$PROXY_PORT"
}

private_app_denies_outsiders() {
  local source status path=${1:-/api}
  for source in 127.0.0.1 203.0.113.77 100.64.0.10; do
    status=$(curl --noproxy '*' --silent --show-error --max-time 3 \
      --haproxy-clientip "$source" --cacert "$TEST_ROOT/tls.pem" \
      --resolve "$APP_ID.mesh.test:$PROXY_PORT:127.0.0.1" \
      --header 'X-Forwarded-For: 100.64.0.9' --header 'X-Forwarded-Proto: https' \
      --header 'Tailscale-User-Login: owner@example.test' --cookie 'mesh_app_session=forged' \
      -o "$TEST_ROOT/private.body" -w '%{http_code}' "$(app_endpoint)$path") || return 1
    [ "$status" = 404 ] || { echo "source $source returned $status, want 404" >&2; return 1; }
    if grep -Eq 'APP_LABELLED_WORKER|"pid"' "$TEST_ROOT/private.body"; then
      echo "source $source received private backend bytes" >&2
      return 1
    fi
  done
}

private_app_rejects_sharing_commands() {
  local command
  for command in public private; do
    if MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app "$command" local "$APP_ID" --json \
      >"$TEST_ROOT/$command.json" 2>"$TEST_ROOT/$command.err"; then
      echo "removed app $command command succeeded" >&2
      return 1
    fi
  done
}
