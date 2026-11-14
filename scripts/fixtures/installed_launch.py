"""Separate command dispatch, rejected admission and possible fixture execution."""
import hashlib
import json
import os
from pathlib import Path


def document(path):
    value = json.loads(Path(path).read_bytes())
    if not isinstance(value, dict):
        raise ValueError("fixture launch evidence must be an object")
    return value


class Launch:
    def __init__(self, path, output):
        self.path = path
        self.value = {"schema": 1, "started": True, "state": "admission-pending", "output": str(output)}
        self.write()

    def write(self):
        if self.path is not None:
            self.path.write_text(json.dumps(self.value, indent=2) + "\n")

    def rejected(self, error, job_clock):
        if self.path is None:
            return
        try:
            raw = job_clock.read_bytes()
            job = document(job_clock)
            require_job(job)
            self.value.update(state="admission-rejected", errorType=type(error).__name__, error=str(error),
                              jobClockSha256=hashlib.sha256(raw).hexdigest(), run=job["run"], job=job["job"])
            self.write()
        except (OSError, ValueError, TypeError, AttributeError) as evidence_error:
            error.add_note("admission rejection evidence unavailable: " + str(evidence_error))

    def runtime_possible(self):
        self.value["state"] = "runtime-possible"
        self.write()


def require_job(job):
    for name in ("run", "job"):
        if not job.get(name) or job[name] != os.environ.get("GITHUB_RUN_ID" if name == "run" else "GITHUB_JOB"):
            raise ValueError("fixture launch evidence belongs to another job")
    for name in ("origin_ns", "admission_ns"):
        if type(job.get(name)) is not int or job[name] <= 0:
            raise ValueError("fixture launch evidence has an invalid original job clock")


def rejected_before_runtime(root, launch):
    if type(launch.get("schema")) is not int or launch["schema"] != 1 or launch.get("started") is not True or launch.get("state") != "admission-rejected":
        return False
    if launch.get("errorType") != "ClockExpired" or not isinstance(launch.get("error"), str) or not launch["error"]:
        return False
    if launch.get("output") != str((root / "cohort-proof").resolve()):
        return False
    job_clock = root / "installed-job-clock.json"
    job = document(job_clock)
    require_job(job)
    if any(launch.get(name) != job[name] for name in ("run", "job")):
        return False
    if launch.get("jobClockSha256") != hashlib.sha256(job_clock.read_bytes()).hexdigest():
        return False
    control = root / "cohort-control"
    manager, scratch = document(control / "native-manager.json"), document(control / "scratch-cleanup.json")
    cleanup = manager.get("cleanup", {})
    return (type(manager.get("schema")) is int and manager["schema"] == 1 and manager.get("passed") is True
            and cleanup.get("serviceAbsent") is True and cleanup.get("processAbsent") is True and scratch.get("absent") is True
            and isinstance(scratch.get("root"), str) and Path(scratch["root"]).is_absolute()
            and manager.get("created", {}).get("root") == scratch["root"])
