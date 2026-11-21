#!/usr/bin/env python3
"""Hold a pull request's Mac legs until no main CI run is queued or running.

GitHub hands the Mac runners' next jobs to any queued jobs, so a stream
of pull request runs can keep a main publish run waiting for hours. Main CI
runs, manual runs, fork pull requests and drafts pass at once. A same-repository
pull request waits here, on a hosted runner, until main is idle. If main is
still active at the deadline the job fails, so the PR's Mac legs skip and the
PR is rerun once main is idle; proceeding would put it on the Mac against main.

GitHub's status-filtered run lists can miss an active run between its job
phases, so the wait reads the newest main runs unfiltered, counts every run
that is not completed, and needs main to look idle on two polls in a row.
"""

import json
import os
import subprocess
import sys
import time

POLL_SECONDS = 60
DEADLINE_MINUTES = 180
IDLE_POLLS = 2
RECENT_RUNS = 30


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
    page = api(f"repos/{repository}/actions/workflows/ci.yml/runs?branch=main&per_page={RECENT_RUNS}")
    return [f"{run['id']} ({run['status']})" for run in page["workflow_runs"] if run["status"] != "completed"]


def poll(repository, api):
    try:
        return active_main_runs(repository, api)
    except (RuntimeError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        return [f"unknown ({error})"]


def wait(environment, api=gh_api, sleep=time.sleep, clock=time.monotonic, out=sys.stdout):
    if not must_wait(environment):
        print("Mac turn: no wait for this event", file=out)
        return 0
    repository = environment["REPOSITORY"]
    deadline = clock() + int(environment.get("DEADLINE_MINUTES", DEADLINE_MINUTES)) * 60
    idle = 0
    while True:
        runs = poll(repository, api)
        idle = 0 if runs else idle + 1
        if idle >= IDLE_POLLS:
            print("Mac turn: no main CI run is queued or running", file=out)
            return 0
        if clock() >= deadline:
            minutes = int(environment.get("DEADLINE_MINUTES", DEADLINE_MINUTES))
            print(f"::error::Mac turn: main CI still active after {minutes} minutes; rerun this job when main is idle: "
                  f"{', '.join(runs)}", file=out)
            return 1
        if runs:
            print(f"Mac turn: waiting for main CI: {', '.join(runs)}", file=out, flush=True)
        sleep(POLL_SECONDS)


if __name__ == "__main__":
    sys.exit(wait(os.environ))
