"""Section 11 gateway benchmark: direct `mesh mcp connect` versus the aggregate
`mesh mcp --profile` over one isolated hosting daemon on loopback.

Prints one JSON document with raw samples. Exact output is checked on every
sample. Usage (inside the integration isolation): mcp_gateway.py MESH OUT.json
"""
import json
import os
import platform
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path

HELPERS = Path(__file__).resolve().parents[2] / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))

from mcp_connect import run_mesh, setup_client_device  # noqa: E402
from mcp_gateway import Client, exposed_name, list_all  # noqa: E402
from pipe_execution import PipeFixture  # noqa: E402
from terminal_window import require  # noqa: E402

SERVICES = 10
EXTRA_TOOLS = 39  # 11 fixture tools plus 39 plain ones: 50 per service.
COLD, WARM, BIG = 8, 100, 10
BIG_BYTES = 2 << 20


def ms(start):
    return (time.perf_counter() - start) * 1000


def pct(samples, q):
    ordered = sorted(samples)
    return ordered[min(len(ordered) - 1, int(round(q * (len(ordered) - 1))))]


def summary(samples):
    return {"p50": pct(samples, 0.5), "p95": pct(samples, 0.95), "mean": statistics.fmean(samples)}


def proc_stats(pid):
    status = Path(f"/proc/{pid}/status").read_text()
    rss = next(int(line.split()[1]) for line in status.splitlines() if line.startswith("VmRSS:"))
    fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
    ticks = os.sysconf("SC_CLK_TCK")
    return {"rssKiB": rss, "cpuSeconds": (int(fields[11]) + int(fields[12])) / ticks}


def connections(pid):
    out = subprocess.run(["ss", "-tnpH", "state", "established"], capture_output=True, text=True, check=False).stdout
    return sum(1 for line in out.splitlines() if f"pid={pid}," in line)


def echo(client, name, text):
    reply = client.tool(name, {"text": text})
    require(reply["result"]["content"][0]["text"].endswith(f":echo:{text}") or reply["result"]["content"][0]["text"].endswith(f":{text}"), reply)


def cold(binary, env, args, name):
    samples = []
    for i in range(COLD):
        start = time.perf_counter()
        c = Client(binary, env, args)
        try:
            echo(c, name, f"cold{i}")
            samples.append(ms(start))
        finally:
            c.kill()
    return samples


def warm(client, name):
    samples = []
    for i in range(WARM):
        start = time.perf_counter()
        echo(client, name, f"w{i}")
        samples.append(ms(start))
    return samples


def big(client, name):
    rates = []
    for _ in range(BIG):
        start = time.perf_counter()
        reply = client.tool(name, {"bytes": BIG_BYTES})
        elapsed = time.perf_counter() - start
        require(len(reply["result"]["content"][0]["text"]) == BIG_BYTES, "big reply size")
        rates.append(BIG_BYTES / elapsed / (1 << 20))
    return rates


def main():
    binary, out = sys.argv[1], sys.argv[2]
    root = Path(os.environ["HOME"]).parent / "mcp-gateway-bench"
    (root / "host").mkdir(parents=True)
    fixture = PipeFixture(binary, root / "host")
    try:
        result = run(binary, root, fixture)
    finally:
        fixture.close()
    Path(out).write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: v for k, v in result.items() if k != "raw"}, indent=2))


def run(binary, root, fixture):
    fixture.start_remote()
    entry = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]
    claim = json.loads((fixture.config / "machine-names" / f"{fixture.remote_id}.json").read_text())
    host_env = dict(fixture.environment, MESH_CONFIG_DIR=str(fixture.root / "remote-config"), MESH_STATE_DIR=str(fixture.remote),
                    GW_EXTRA_TOOLS=str(EXTRA_TOOLS))
    host = fixture.remote_id
    upstream = [sys.executable, "-B", str(HELPERS / "mcp_gateway_upstream.py")]
    services = []
    for i in range(SERVICES):
        out = run_mesh(binary, host_env, "mcp", "publish", f"svc{i}", "--shared", "--env", f"GW_EXTRA_TOOLS={EXTRA_TOOLS}",
                       "--json", "--", *upstream, f"svc{i}", check_rc=0).stdout
        services.append(json.loads(out)["data"]["id"])
    env, cfg, state, device = setup_client_device(binary, root, "client", [entry], {host: claim})
    run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", device, check_rc=0)
    run_mesh(binary, env, "mcp", "identity", "create", "bench", "--json", check_rc=0)
    run_mesh(binary, env, "mcp", "profile", "add", "one", f"{host}/svc0", "--native", "none", "--json", check_rc=0)
    run_mesh(binary, env, "mcp", "profile", "add", "all", "--native", "all", "--json", check_rc=0)
    for i in range(SERVICES):
        run_mesh(binary, env, "mcp", "profile", "add", "all", f"{host}/svc{i}", "--json", check_rc=0)

    echo_name = exposed_name(host, services[0], "t", "echo")
    big_name = exposed_name(host, services[0], "t", "big")
    direct_args = ["connect", f"{host}/svc0", "--identity", "bench"]
    gateway_args = ["--profile", "one", "--identity", "bench"]
    # Start the hosted instance once so cold samples measure the frontend.
    warmup = Client(binary, env, direct_args)
    echo(warmup, "echo", "start")
    warmup.kill()

    raw = {"coldDirect": cold(binary, env, direct_args, "echo"), "coldGateway": cold(binary, env, gateway_args, echo_name)}
    direct, gw = Client(binary, env, direct_args), Client(binary, env, gateway_args)
    try:
        raw["warmDirect"], raw["warmGateway"] = warm(direct, "echo"), warm(gw, echo_name)
        raw["bigDirectMiBs"], raw["bigGatewayMiBs"] = big(direct, "big"), big(gw, big_name)
    finally:
        direct.kill()
        gw.kill()

    discovery = discover_all(binary, env, state)
    return {
        "commit": subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=False, cwd=HELPERS).stdout.strip(),
        "goVersion": subprocess.run(["go", "version"], capture_output=True, text=True, check=False).stdout.strip(),
        "platform": f"{platform.system().lower()}/{platform.machine()}", "loadAvg": os.getloadavg(),
        "params": {"services": SERVICES, "toolsPerService": 11 + EXTRA_TOOLS, "cold": COLD, "warm": WARM, "big": BIG, "bigBytes": BIG_BYTES},
        "coldDirectMs": summary(raw["coldDirect"]), "coldGatewayMs": summary(raw["coldGateway"]),
        "warmDirectMs": summary(raw["warmDirect"]), "warmGatewayMs": summary(raw["warmGateway"]),
        "bigDirectMiBs": summary(raw["bigDirectMiBs"]), "bigGatewayMiBs": summary(raw["bigGatewayMiBs"]),
        "daemon": proc_stats(fixture.daemon.pid),
        "discovery": discovery, "raw": raw,
    }


def discover_all(binary, env, state):
    """First listing of 10 services with an empty cache (services already
    running), then a fresh frontend with the private cache warm."""
    shutil.rmtree(Path(state) / "mcp-gateway-cache", ignore_errors=True)
    out = {}
    for label in ("emptyCache", "warmCache"):
        gw = Client(binary, env, ["--profile", "all", "--identity", "bench"])
        try:
            start = time.perf_counter()
            tools, _ = list_all(gw, "tools/list", "tools")
            elapsed = ms(start)
            hosted = [t for t in tools if t["name"].startswith(("h_", "hx_"))]
            if len(hosted) != SERVICES * (11 + EXTRA_TOOLS):
                gw.kill()
                require(False, f"listed {len(hosted)} hosted tools: {gw.proc.stderr.read().decode(errors='replace')}")
            start = time.perf_counter()
            again, _ = list_all(gw, "tools/list", "tools")
            relist = ms(start)
            require(again == tools, "relist differs")
            out[label] = {"listMs": elapsed, "relistMs": relist, "tools": len(tools),
                          "catalogBytes": sum(len(json.dumps(t, separators=(",", ":"))) for t in tools),
                          "connections": connections(gw.proc.pid), "frontend": proc_stats(gw.proc.pid)}
        finally:
            gw.kill()
    cache = Path(state) / "mcp-gateway-cache"
    out["cacheBytes"] = sum(p.stat().st_size for p in cache.glob("*.json"))
    return out


if __name__ == "__main__":
    main()
