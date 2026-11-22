"""Drive mesh mcp publish/ls/show/start/stop/restart/logs/rm against a real daemon."""
import json
import os
from pathlib import Path
import subprocess
import sys

from pipe_execution import PipeFixture
from terminal_window import print_diagnostics, require

# A minimal stateless MCP 2026-07-28 server: it answers server/discover and
# writes one diagnostic line to stderr.
SERVER = r"""#!/bin/sh
echo "hosted fixture stderr line" >&2
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\("[^"]*"\).*/\1/p')
  case $line in
  *'"server/discover"'*)
    printf '{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":0,"cacheScope":"private"}}\n' "$id" ;;
  esac
done
"""


def mesh(fixture, *arguments, ok=True):
    result = subprocess.run([fixture.binary, "mcp", *arguments], env=fixture.environment,
                            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=60)
    if ok:
        require(result.returncode == 0, f"mesh mcp {arguments} failed: {result.stderr}")
    return result


def show(fixture, name):
    return json.loads(mesh(fixture, "show", name, "--json").stdout)


def main():
    root = Path(os.environ["HOME"]).parent / "mcp-hosted"
    root.mkdir()
    fixture = PipeFixture(sys.argv[1], root)
    server = root / "server.sh"
    server.write_text(SERVER)
    server.chmod(0o700)
    try:
        fixture.start_local_daemon()
        out = mesh(fixture, "publish", "shtools", "--cwd", str(root), "--", "/bin/sh", str(server)).stdout
        require("published shtools revision 1" in out, out)
        require("shtools" in mesh(fixture, "ls").stdout, "ls does not list the service")
        require(show(fixture, "shtools")["instances"] == [], "an instance started before any use")

        require("ready" in mesh(fixture, "start", "shtools").stdout, "start did not reach protocol readiness")
        instance = show(fixture, "shtools")["instances"][0]
        require(instance["state"] == "ready" and instance["generation"] == 1, instance)
        require("hosted fixture stderr line" in mesh(fixture, "logs", "shtools").stdout, "logs did not show stderr")

        mesh(fixture, "restart", "shtools")
        require(show(fixture, "shtools")["instances"][0]["generation"] == 2, "restart kept the old generation")
        require("stopped" in mesh(fixture, "stop", "shtools").stdout, "stop did not stop the instance")

        refused = mesh(fixture, "publish", "oauth", "--account-mode", "per-user", "--", "/bin/true", ok=False)
        require(refused.returncode != 0 and "per-user OAuth" in refused.stderr, refused.stderr)
        refused = mesh(fixture, "publish", "remote", "--url", "http://10.0.0.1/mcp", "--shared", ok=False)
        require(refused.returncode != 0, "a non-loopback URL was accepted")

        mesh(fixture, "rm", "shtools")
        require("shtools" not in mesh(fixture, "ls").stdout, "rm left the service listed")
    except BaseException:
        print_diagnostics(fixture)
        raise
    finally:
        fixture.close()
    print("PASS: mesh mcp publish, ls, show, start, logs, restart, stop and rm on a real daemon")


if __name__ == "__main__":
    main()
