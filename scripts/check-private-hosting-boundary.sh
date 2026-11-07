#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
source scripts/private-boundary-search.sh
for retired in internal/edge internal/tunnel integration/fixtures/public-edge-renewer.pem; do
  if [[ -e "$retired" ]]; then
    printf 'FAIL: retired public hosting source remains: %s\n' "$retired" >&2
    exit 1
  fi
done
require_no_boundary_match 'Mesh retains a public hosting dependency or contract' \
  --include='*.go' --exclude='*_test.go' --exclude-dir=apppill -- 'internal/(edge|tunnel)|PublicEdgeTarget|ProfilePublicEdge|ConfirmPublic|TypeEdge(Register|List)|TypeTunnelClaim|json:"publicName|json:"wakeOnRequest|AppRegistryExchange|trustPublicEdgeForwarding' cmd internal
require_no_boundary_match 'Mesh retains a public hosting command or flag' \
  -- '"public-edge-target"|"edge"|"public"|"wake-on-request"|"claim"|"release"' internal/cli/command.go internal/cli/serve_command.go
scripts/check-private-app-boundary.sh
printf '%s\n' 'PASS: Mesh hosting is private; public gateways, publishers and tunnels are absent'
