#!/usr/bin/env python3
"""Serial, bounded uploads to the exact candidate draft, using gh's authority."""
import argparse
from dataclasses import dataclass
from email.utils import parsedate_to_datetime
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import urlencode

from fixtures import draft_intake as intake
from fixtures import release_documents

MAX_SECONDS = 1800
MAX_RETRIES = 12
ASSET_RETRIES = 2


class Reconcile(Exception):
    pass


@dataclass
class Response:
    status: int
    headers: dict
    body: object


def response(output):
    head, separator, body = output.replace(b"\r\n", b"\n").partition(b"\n\n")
    lines = head.decode().splitlines()
    if not separator or not lines or not re.fullmatch(r"HTTP/\S+ \d{3}.*", lines[0]):
        raise ValueError("gh api did not return an HTTP response")
    headers = dict(line.lower().split(": ", 1) for line in lines[1:] if ": " in line)
    status = int(lines[0].split()[1])
    try:
        value = json.loads(body) if body.strip() else None
    except json.JSONDecodeError:
        if 200 <= status < 300:
            raise ValueError("gh api returned invalid JSON") from None
        value = None
    return Response(status, headers, value)


def retry_wait(reply, attempt, now):
    headers = reply.headers
    message = reply.body.get("message", "") if isinstance(reply.body, dict) else ""
    limited = reply.status in (403, 429) and (
        reply.status == 429 or headers.get("x-ratelimit-remaining") == "0" or "secondary rate limit" in message.lower())
    if not limited and reply.status not in (502, 503, 504):
        return None
    waits = [60 * 2 ** attempt]
    if "retry-after" in headers:
        value = headers["retry-after"]
        waits.append(float(value) if value.isdigit() else parsedate_to_datetime(value).timestamp() - now)
    if headers.get("x-ratelimit-remaining") == "0":
        waits.append(float(headers["x-ratelimit-reset"]) - now + 1)
    if any(not math.isfinite(value) for value in waits):
        raise ValueError("invalid rate limit wait")
    return max(waits)


class API:
    def __init__(self):
        self.deadline = time.monotonic() + MAX_SECONDS
        self.next_mutation = 0
        self.retries = 0

    def remaining(self):
        value = self.deadline - time.monotonic()
        if value <= 0:
            raise ValueError("draft upload exceeded its 30-minute wall clock bound")
        return value

    def wait(self, seconds):
        if seconds >= self.remaining():
            raise ValueError("server wait exceeds the remaining draft upload wall clock bound; draft retained")
        if seconds > 0:
            time.sleep(seconds)

    def backoff(self, seconds):
        if self.retries >= MAX_RETRIES:
            raise ValueError("draft upload exhausted its total retry bound; draft retained")
        self.retries += 1
        print(f"Draft upload retry {self.retries}/{MAX_RETRIES}; waiting {math.ceil(seconds)} seconds", flush=True)
        self.wait(seconds)

    def once(self, method, endpoint, data=None):
        if method != "GET":
            self.wait(max(0, self.next_mutation - time.monotonic()))
        command = ["gh", "api", "--hostname", "github.com", "--include", "--method", method, endpoint,
                   "-H", "Accept: application/vnd.github+json"]
        if data is not None:
            command += ["-H", "Content-Type: application/octet-stream", "--input", "-"]
        env = dict(os.environ)
        env.pop("GH_DEBUG", None)
        try:
            result = subprocess.run(command, input=data, capture_output=True, env=env,
                                    timeout=min(120, self.remaining()), check=False)
        except subprocess.TimeoutExpired:
            return Response(504, {}, None)
        finally:
            if method != "GET":
                self.next_mutation = time.monotonic() + 1
        reply = response(result.stdout)
        if result.returncode and 200 <= reply.status < 300:
            raise ValueError("gh api failed despite a successful HTTP status")
        return reply

    def request(self, method, endpoint, data=None, retry_number=0):
        for attempt in range(ASSET_RETRIES + 1):
            reply = self.once(method, endpoint, data)
            if 200 <= reply.status < 300:
                return reply.body
            seconds = retry_wait(reply, attempt + retry_number, time.time())
            if method != "GET" and retry_number == ASSET_RETRIES:
                raise ValueError(f"draft asset exhausted its retry bound at HTTP {reply.status}; draft retained")
            if seconds is None or attempt == ASSET_RETRIES:
                raise ValueError(f"draft upload HTTP {reply.status}; draft retained")
            self.backoff(seconds)
            if method != "GET":
                raise Reconcile
        raise AssertionError("unreachable")


def local_assets(paths):
    assets = {}
    for path in paths:
        absolute = path.absolute()
        if (absolute != path.resolve() or not path.is_file()
                or not any(absolute.is_relative_to(Path.cwd() / root) for root in ("dist", "proofs"))
                or path.name not in release_documents.PHYSICAL and not release_documents.RECEIPT.fullmatch(path.name)):
            raise ValueError("signed upload requires the declared fixed physical assets under dist or proofs")
        if path.name in assets or not 0 < path.stat().st_size <= 128 << 20:
            raise ValueError("signed upload has a duplicate name or invalid file size")
        data = path.read_bytes()
        assets[path.name] = (path, len(data), "sha256:" + hashlib.sha256(data).hexdigest())
    maximum = len(release_documents.PHYSICAL) + len(release_documents.legacy_versions()) * len(intake.ARCHIVES)
    if not set(release_documents.PHYSICAL) <= assets.keys() or len(assets) > maximum:
        raise ValueError("signed upload lacks its fixed assets or exceeds the supported legacy projection")
    return assets


def exact(asset, local):
    return (asset.get("state") == "uploaded" and asset.get("size") == local[1]
            and asset.get("digest") == local[2])


class Publisher:
    def __init__(self, api, candidate, version, source, paths):
        if os.environ.get("GITHUB_REPOSITORY") != "ShaulLavo/mesh":
            raise ValueError("signed upload requires the release writer repository")
        self.candidate = intake.verify(candidate, version, source, run=os.environ["GITHUB_RUN_ID"])
        self.version, self.source = version, source
        self.assets = local_assets(paths)
        if set(self.assets) & {item["name"] for item in self.candidate["assets"]}:
            raise ValueError("signed upload cannot replace a transition candidate asset")
        self.api = api
        self.release = f'{intake.REPOSITORY}/releases/{self.candidate["id"]}'

    def validate(self):
        release = self.api.request("GET", self.release)
        release.update(repository="ShaulLavo/mesh", intake_run=self.candidate["intake_run"])
        intake.identity(release, self.version, self.source)
        if release["id"] != self.candidate["id"]:
            raise ValueError("signed upload differs from the reserved release ID")

    def inventory(self):
        self.validate()
        assets = []
        for page in range(1, 330):
            batch = self.api.request("GET", self.release + f"/assets?per_page=100&page={page}")
            assets.extend(batch)
            if len(batch) < 100:
                break
        allowed = set(self.assets) | set(intake.NAMES) | set(self.candidate["frozen_baselines"]) | {"mesh.rb"}
        if len(assets) > len(allowed) or len({item["name"] for item in assets}) != len(assets) or any(item["name"] not in allowed for item in assets):
            raise ValueError("draft upload inventory exceeds its bound or repeats names")
        by_name = {item["name"]: item for item in assets}
        for item in self.candidate["assets"]:
            self.validate_candidate_asset(by_name.get(item["name"]), item)
        return by_name

    @staticmethod
    def validate_candidate_asset(live, original):
        keys = ("id", "name", "size", "state", "digest")
        if live is None or any(live.get(key) != original[key] for key in keys):
            raise ValueError("draft upload differs from the exact transition candidate")

    def mutate(self, method, endpoint, data=None):
        self.validate()
        return self.api.request(method, endpoint, data, retry_number=self.attempt)

    def upload(self, name, inventory):
        local = self.assets[name]
        for attempt in range(ASSET_RETRIES + 1):
            self.attempt = attempt
            try:
                self.store(name, local, inventory)
                return
            except Reconcile:
                pass
            if attempt == ASSET_RETRIES:
                raise ValueError("draft asset exhausted its retry bound; draft retained") from None
            inventory = self.inventory()

    def store(self, name, local, inventory):
        old = inventory.get(name)
        if old is not None and exact(old, local):
            return
        # Only declared physical assets may be replaced in the exact candidate draft.
        data = local[0].read_bytes()
        if len(data) != local[1] or "sha256:" + hashlib.sha256(data).hexdigest() != local[2]:
            raise ValueError("signed upload local bytes changed after validation")
        if old is not None:
            if type(old.get("id")) is not int or old["id"] <= 0 or old.get("state") not in ("uploaded", "starter"):
                raise ValueError("refusing to replace an invalid draft asset")
            self.mutate("DELETE", f'{intake.REPOSITORY}/releases/assets/{old["id"]}')
        endpoint = "https://uploads.github.com/" + self.release + "/assets?" + urlencode({"name": name})
        uploaded = self.mutate("POST", endpoint, data)
        intake.validate_asset(uploaded)
        if uploaded["name"] != name or not exact(uploaded, local):
            raise ValueError("uploaded draft asset differs from the exact local bytes")

    def run(self):
        inventory = self.inventory()
        for number, name in enumerate(self.assets, 1):
            self.upload(name, inventory)
            print(f"Draft asset {number}/{len(self.assets)} verified: {name}", flush=True)
        inventory = self.inventory()
        if any(not exact(inventory.get(name, {}), local) for name, local in self.assets.items()):
            raise ValueError("final draft inventory differs from the exact signed files")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("assets_json", type=Path)
    args = parser.parse_args()
    try:
        paths = [Path(item) for item in json.loads(args.assets_json.read_bytes())]
        Publisher(API(), args.candidate, args.version, args.source, paths).run()
    except (ValueError, KeyError, OSError) as error:
        raise SystemExit(f"signed draft upload: {error}") from None


if __name__ == "__main__":
    main()
