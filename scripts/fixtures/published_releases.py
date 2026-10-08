"""Public release inputs verified before disposable native execution."""
import hashlib
import io
import itertools
import json
import socket
import ssl
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

VERSIONS = ("v0.1.149", "v0.1.151", "v0.1.159")
ORIGIN = "https://github.com/ShaulLavo/mesh/releases/download/"
COMPATIBILITY_FIELDS = ("stateReadMin", "stateReadMax", "stateWrite", "workerMin", "workerMax", "workerWrite", "journalVersion")
DOWNLOAD_BUDGET = 60
# GitHub release 5xx bursts outlast sub-second backoff. Capping each attempt lets a
# stalled connection be retried; three attempts and both waits fit the total budget.
ATTEMPT_LIMIT = 20
DOWNLOAD_BACKOFF = (2, 6)
DOWNLOAD_WORKER = (sys.executable, str(Path(__file__).resolve()))


def digest(data):
    return hashlib.sha256(data).hexdigest()


def download(address, maximum):
    asset = describe(address)
    deadline = time.monotonic() + DOWNLOAD_BUDGET
    attempts = len(DOWNLOAD_BACKOFF) + 1
    for attempt in range(1, attempts + 1):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(f"published release acquisition of {asset} exceeded its total deadline")
        data, detail, transient, stalled = download_attempt(address, maximum, min(deadline, time.monotonic() + ATTEMPT_LIMIT))
        if data is not None:
            if time.monotonic() >= deadline:
                raise TimeoutError(f"published release acquisition of {asset} exceeded its total deadline")
            return data
        failure = f"{asset} after {attempt} attempt{'s' if attempt > 1 else ''}: {detail}"
        if stalled and (attempt == attempts or time.monotonic() >= deadline):
            raise TimeoutError(f"published release acquisition exceeded its total deadline for {failure}")
        if not transient or attempt == attempts:
            raise RuntimeError(f"published release acquisition failed for {failure}")
        delay = DOWNLOAD_BACKOFF[attempt - 1]
        if deadline - time.monotonic() <= delay:
            raise TimeoutError(f"published release acquisition exceeded its total deadline for {failure}")
        time.sleep(delay)


def describe(address):
    parts = urllib.parse.urlsplit(address)
    return f"{'/'.join(parts.path.split('/')[-2:])} from {parts.hostname}"


def download_attempt(address, maximum, end):
    limit = end - time.monotonic()
    # Socket timeouts cannot bound blocking DNS or repeated response reads.
    with subprocess.Popen((*DOWNLOAD_WORKER, address, str(maximum), str(limit)),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE) as worker:
        try:
            # Launch time counts against the attempt, so the wait is measured after Popen returns.
            data, failure = worker.communicate(timeout=max(0, end - time.monotonic()))
        except subprocess.TimeoutExpired:
            worker.kill()
            worker.communicate()
            # A stalled attempt is retried while the total budget can still fit another one.
            return None, f"attempt stalled past {limit:.1f} s", True, True
    if worker.returncode == 0:
        return data, None, False, False
    return None, failure.decode(errors="replace").strip(), worker.returncode == 75, False


def download_worker(address, maximum, timeout):
    try:
        data = download_once(address, int(maximum), float(timeout))
    except Exception as error:
        cause = error
        while isinstance(cause, urllib.error.URLError) and not isinstance(cause, urllib.error.HTTPError):
            cause = cause.reason
        transient = isinstance(cause, ConnectionResetError)
        if isinstance(cause, socket.gaierror):
            transient = cause.errno in (socket.EAI_AGAIN, socket.EAI_NONAME)
        if isinstance(cause, urllib.error.HTTPError):
            transient = cause.geturl().startswith("https://") and cause.code in (500, 502, 503, 504)
            cause.close()
        source = f" ({urllib.parse.urlsplit(cause.geturl()).hostname})" if isinstance(cause, urllib.error.HTTPError) else ""
        print(f"{type(error).__name__}: {error}{source}", file=sys.stderr)
        raise SystemExit(75 if transient else 1) from error
    sys.stdout.buffer.write(data)


class HTTPSRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, address):
        if not address.startswith("https://"):
            response.close()
            raise RuntimeError("published release redirected outside HTTPS")
        return super().redirect_request(request, response, code, message, headers, address)


def download_once(address, maximum, timeout):
    # Fixture proxies and certificate files belong to children, never public acquisition.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=ssl.create_default_context()), HTTPSRedirectHandler())
    with opener.open(address, timeout=timeout) as response:
        if not response.url.startswith("https://"):
            raise RuntimeError("published release redirected outside HTTPS")
        data = response.read(maximum + 1)
    if len(data) > maximum:
        raise RuntimeError("published release asset exceeds its size limit")
    return data


def fetch(root, version, name, maximum, fetcher):
    if Path(name).name != name:
        raise RuntimeError("published asset name is not a basename")
    path = root / version / name
    data = path.read_bytes() if path.exists() else fetcher(ORIGIN + version + "/" + name, maximum)
    if len(data) > maximum:
        raise RuntimeError("published release asset exceeds its size limit")
    return data


def archive_binary(data, artifact):
    if digest(data) != artifact["sha256"]:
        raise RuntimeError("published archive digest mismatch")
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
        members = [member for member in archive.getmembers() if member.name in ("mesh", "./mesh")]
        if len(members) != 1 or not members[0].isfile() or members[0].size > 128 << 20:
            raise RuntimeError("published archive requires one bounded regular Mesh executable")
        binary = archive.extractfile(members[0]).read()
    if digest(binary) != artifact["binarySha256"]:
        raise RuntimeError("published executable digest mismatch")
    return binary


def joined_receipt(data, transition, compatibility):
    if digest(data) != transition["proof"]:
        raise RuntimeError("published receipt digest mismatch")
    receipt = json.loads(data)
    if receipt["schema"] != 1:
        raise RuntimeError("published receipt schema mismatch")
    for field in ("platform", "fromDigest", "toDigest"):
        if receipt[field] != transition[field]:
            raise RuntimeError("published receipt transition mismatch")
    for field in COMPATIBILITY_FIELDS:
        if receipt[field] != compatibility[field]:
            raise RuntimeError("published receipt compatibility mismatch")
    for field in ("retainedOpenedCandidateState", "sessionsPreserved", "recoveryRecordsPreserved"):
        if receipt[field] is not True:
            raise RuntimeError("published receipt does not declare retained state and sessions")
    return receipt


def acquire(root, platform, fetcher=download):
    root = Path(root)
    releases = []
    executables = {}
    evidence = {"acquisition": "ordinary system-trusted public GitHub HTTPS", "archives": [], "bridges": []}
    for version in VERSIONS:
        (root / version).mkdir(parents=True, exist_ok=True)
        raw = fetch(root, version, "mesh-release.json", 1 << 20, fetcher)
        manifest = json.loads(raw)
        if manifest["schema"] != 1 or manifest["version"] != version:
            raise RuntimeError("published manifest version or schema mismatch")
        files = {"mesh-release.json": raw}
        executable = None
        for artifact in manifest["artifacts"]:
            data = fetch(root, version, artifact["archive"], 128 << 20, fetcher)
            binary = archive_binary(data, artifact)
            files[artifact["archive"]] = data
            evidence["archives"].append({"version": version, **artifact})
            if artifact["platform"] == platform:
                executable = root / version / "mesh"
                executables[executable] = binary
        if executable is None:
            raise RuntimeError("published release has no native platform artifact")
        releases.append({"manifest": manifest, "files": files, "executable": executable})
    for previous, target in itertools.pairwise(releases):
        for artifact in previous["manifest"]["artifacts"]:
            platform = artifact["platform"]
            candidates = [entry for entry in target["manifest"]["artifacts"] if entry["platform"] == platform]
            if len(candidates) != 1:
                raise RuntimeError("published bridge requires one target artifact per platform")
            transitions = [entry for entry in target["manifest"]["compatibility"]["transitions"]
                if entry["platform"] == platform and entry["fromDigest"] == artifact["binarySha256"]
                and entry["toDigest"] == candidates[0]["binarySha256"]]
            if len(transitions) != 1:
                raise RuntimeError("published bridge requires one joined transition")
            transition = transitions[0]
            name = transition["proof"] + ".json"
            data = fetch(root, target["manifest"]["version"], name, 1 << 20, fetcher)
            joined_receipt(data, transition, target["manifest"]["compatibility"])
            target["files"][name] = data
            evidence["bridges"].append({"fromVersion": previous["manifest"]["version"],
                "toVersion": target["manifest"]["version"], **transition})
    # Publish cached inputs only after the complete bridge chain verifies.
    for row in releases:
        for name, data in row["files"].items():
            (root / row["manifest"]["version"] / name).write_bytes(data)
    for executable, binary in executables.items():
        executable.write_bytes(binary)
        executable.chmod(0o755)
    (root / "verified.json").write_text(json.dumps(evidence, indent=2) + "\n")
    return releases


if __name__ == "__main__":
    download_worker(*sys.argv[1:])
