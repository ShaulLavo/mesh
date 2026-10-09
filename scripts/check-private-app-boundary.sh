#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
if rg -n 'apppill\.|internal/apppill|SetAppHandler|serveApp\(|AcquireApp|AppClientIP|TypeAppEdge|app\.edge' internal --glob '*.go' --glob '!**/*_test.go' --glob '!**/apppill/**'; then
  echo 'FAIL: temporary apps retain a widget or public dispatch dependency' >&2
  exit 1
fi
if rg -n 'Visibility|json:"visibility' internal/apps internal/cli/app.go --glob '*.go' --glob '!**/*_test.go'; then
  echo 'FAIL: temporary apps retain a visibility field' >&2
  exit 1
fi
if rg -n 'mesh-app-status|data-mesh-manager|Make public|Make private|/confirm\?id=|visibility:[[:space:]]*(.public|.private)' web/app-pill/src internal/apppill/assets; then
  echo 'FAIL: the reusable widget retains Mesh sharing controls' >&2
  exit 1
fi
echo 'PASS: app dispatch, app fields and reusable widget have no sharing dependency'
