"""Separate validation failure from possible fixture execution."""
import json
from pathlib import Path


def document(path):
    value = json.loads(Path(path).read_bytes())
    if not isinstance(value, dict):
        raise ValueError("fixture launch evidence must be an object")
    return value


class Launch:
    def __init__(self, path, output):
        self.path = path
        self.value = {"schema": 1, "started": True, "state": "validation-pending", "output": str(output)}
        self.write()

    def write(self):
        if self.path is not None:
            self.path.write_text(json.dumps(self.value, indent=2) + "\n")

    def not_started(self, error):
        if self.path is None:
            return
        self.value.update(state="validation-failed", errorType=type(error).__name__, error=str(error))
        try:
            self.write()
        except OSError as evidence_error:
            error.add_note("pre-runtime launch evidence unavailable: " + str(evidence_error))

    def runtime_possible(self):
        self.value["state"] = "runtime-possible"
        self.write()


def before_runtime(root, launch):
    if type(launch.get("schema")) is not int or launch["schema"] != 1 or launch.get("started") is not True or launch.get("state") != "validation-failed":
        return False
    if not isinstance(launch.get("errorType"), str) or not launch["errorType"] or not isinstance(launch.get("error"), str) or not launch["error"]:
        return False
    if launch.get("output") != str((root / "cohort-proof").resolve()):
        return False
    control = root / "cohort-control"
    manager, scratch = document(control / "native-manager.json"), document(control / "scratch-cleanup.json")
    cleanup = manager.get("cleanup", {})
    return (type(manager.get("schema")) is int and manager["schema"] == 1 and manager.get("passed") is True
            and cleanup.get("serviceAbsent") is True and cleanup.get("processAbsent") is True and scratch.get("absent") is True
            and isinstance(scratch.get("root"), str) and Path(scratch["root"]).is_absolute()
            and manager.get("created", {}).get("root") == scratch["root"])
