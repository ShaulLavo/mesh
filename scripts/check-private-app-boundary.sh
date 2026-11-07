#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
source scripts/private-boundary-search.sh
require_no_boundary_match 'temporary apps retain a widget or public dispatch dependency' \
  --include='*.go' --exclude='*_test.go' --exclude-dir=apppill -- 'apppill\.|internal/apppill|SetAppHandler|serveApp\(|AcquireApp|AppClientIP|TypeAppEdge|app\.edge' internal
require_no_boundary_match 'temporary apps retain a visibility field' \
  --include='*.go' --exclude='*_test.go' -- 'Visibility|json:"visibility' internal/apps internal/cli/app.go
require_no_boundary_match 'the reusable widget retains Mesh sharing controls' \
  -- 'mesh-app-status|data-mesh-manager|Make public|Make private|/confirm\?id=|visibility:[[:space:]]*(.public|.private)' web/app-pill/src internal/apppill/assets
echo 'PASS: app dispatch, app fields and reusable widget have no sharing dependency'
