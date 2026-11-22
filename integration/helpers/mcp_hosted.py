"""Drive mesh mcp publish/ls/show/start/stop/restart/logs/rm against a real daemon."""
import json
import os
from pathlib import Path
import subprocess
import sys

from pipe_execution import PipeFixture
from terminal_window import eventually, print_diagnostics, require

# A minimal stateless MCP 2026-07-28 server: it answers server/discover and
# writes one diagnostic line to stderr.
SERVER = r"""#!/bin/sh
echo "hosted fixture stderr line" >&2
printf x >> "$0.launches"
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
    env = json.loads(mesh(fixture, "show", name, "--json").stdout)
    require(env.get("schemaVersion") == 1 and env.get("operation") == "mcp.show" and env.get("status") == "ok", env)
    return env["data"]


def launches(server):
    path = Path(str(server) + ".launches")
    return len(path.read_bytes()) if path.exists() else 0


def crash_during_restart(fixture, server, stage):
    """Kill the daemon at a launch boundary of the next generation, then restart it."""
    instance = show(fixture, "shtools")["instances"][0]
    before = launches(server)
    hold = fixture.local / (".pipe-hold-" + stage)
    hold.write_text(f"{instance['id']}/{instance['generation'] + 1}")
    reached = Path(str(hold) + ".reached")
    restart = subprocess.Popen([fixture.binary, "mcp", "restart", "shtools"], env=fixture.environment,
                               stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        eventually(reached.exists, f"{stage} was not reached")
        fixture.crash_daemon()
    finally:
        restart.wait(timeout=30)
        hold.unlink()
        reached.unlink(missing_ok=True)
    fixture.start_local_daemon()
    # Recovery runs in the background; wait until it settles the instance.
    def settled():
        instance = show(fixture, "shtools")["instances"][0]
        return instance if instance["state"] != "starting" else None
    return before, eventually(settled, "recovery did not settle the instance")


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
        # The worker captures stderr asynchronously, so wait for the line.
        eventually(lambda: "hosted fixture stderr line" in mesh(fixture, "logs", "shtools").stdout, "logs did not show stderr")

        mesh(fixture, "restart", "shtools")
        require(show(fixture, "shtools")["instances"][0]["generation"] == 2, "restart kept the old generation")

        # Crash after the worker started but before its publication: the
        # restarted daemon adopts that exact worker and launches nothing new.
        before, instance = crash_during_restart(fixture, server, "before_mcp_publication")
        require(instance["state"] == "ready" and instance["generation"] == 3, instance)
        require(launches(server) == before + 1, "the published worker was launched twice")
        print("PASS: a worker started before publication is adopted after a daemon crash", flush=True)

        # Crash after dispatch but before the launch: whether a worker exists is
        # unknown, so Mesh refuses a second launch instead of guessing.
        before, instance = crash_during_restart(fixture, server, "before_mcp_worker_launch")
        require(instance["state"] == "uncertain", instance)
        refused = mesh(fixture, "start", "shtools", "--instance", instance["id"], ok=False)
        require(refused.returncode != 0 and "service.unavailable" in refused.stderr, refused.stderr)
        require(launches(server) == before, "an uncertain launch was repeated")
        print("PASS: an uncertain launch is reported and never repeated", flush=True)

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
