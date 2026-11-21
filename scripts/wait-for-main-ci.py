#!/usr/bin/env python3
"""Hold a pull request's Mac legs until no main CI run is queued or running.

GitHub hands the Mac runners' next jobs to any queued jobs, so a stream
of pull request runs can keep a main publish run waiting for hours. Main CI
runs, manual runs, fork pull requests and drafts pass at once. A same-repository
pull request waits here, on a hosted runner, until main is idle. After the
deadline it proceeds with a warning: this is a hang cap, not a gate.
"""

import json
import os
import subprocess
import sys
import time

ACTIVE = ("queued", "in_progress", "waiting", "requested", "pending")
POLL_SECONDS = 60
DEADLINE_MINUTES = 180


def must_wait(environment):
    if environment.get("EVENT") != "pull_request":
        return False
    if environment.get("DRAFT") == "true":
        return False
    return environment.get("HEAD_REPOSITORY") == environment.get("REPOSITORY")


def gh_api(endpoint):
    result = subprocess.run(["gh", "api", endpoint], capture_output=True, text=True, timeout=60, check=False)
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or f"gh api exited {result.returncode}")
    return json.loads(result.stdout)


def active_main_runs(repository, api):
    runs = []
    for status in ACTIVE:
        page = api(f"repos/{repository}/actions/workflows/ci.yml/runs?branch=main&status={status}&per_page=20")
        runs.extend(f"{run['id']} ({run['status']})" for run in page.get("workflow_runs", []))
    return runs


def wait(environment, api=gh_api, sleep=time.sleep, clock=time.monotonic, out=sys.stdout):
    if not must_wait(environment):
        print("Mac turn: no wait for this event", file=out)
        return 0
    repository = environment["REPOSITORY"]
    deadline = clock() + int(environment.get("DEADLINE_MINUTES", DEADLINE_MINUTES)) * 60
    while True:
        try:
            runs = active_main_runs(repository, api)
        except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
            runs = [f"unknown ({error})"]
        if not runs:
            print("Mac turn: no main CI run is queued or running", file=out)
            return 0
        if clock() >= deadline:
            print(f"::warning::Mac turn: main CI still active after the hang cap; proceeding: {', '.join(runs)}", file=out)
            return 0
        print(f"Mac turn: waiting for main CI: {', '.join(runs)}", file=out, flush=True)
        sleep(POLL_SECONDS)


if __name__ == "__main__":
    sys.exit(wait(os.environ))
