#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
if rg -n 'internal/(edge|tunnel)|PublicEdgeTarget|ProfilePublicEdge|ConfirmPublic|TypeEdge(Register|List)|TypeTunnelClaim|json:"publicName|json:"wakeOnRequest|AppRegistryExchange|trustPublicEdgeForwarding' cmd internal --glob '*.go' --glob '!**/*_test.go' --glob '!**/apppill/**'; then
  echo 'FAIL: Mesh retains a public hosting dependency or contract' >&2
  exit 1
fi
if rg -n '"public-edge-target"|"edge"|"public"|"wake-on-request"|"claim"|"release"' internal/cli/command.go internal/cli/serve_command.go; then
  echo 'FAIL: Mesh retains a public hosting command or flag' >&2
  exit 1
fi
scripts/check-private-app-boundary.sh
printf '%s\n' 'PASS: Mesh hosting is private; public gateways, publishers and tunnels are absent'
