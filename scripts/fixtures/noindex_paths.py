"""Assert task-owned Spotlight exclusion before task executables arrive."""
import json
import os
from pathlib import Path
import platform
import subprocess
import sys


def require(path, system=None):
    path = Path(path).resolve(strict=True)
    if (system or platform.system()) != "Darwin":
        return {"path": str(path), "spotlightExclusion": "not applicable"}
    ancestors = [path] + list(path.parents)
    owned = next((parent for parent in ancestors if parent.name.endswith(".noindex")), None)
    if owned is None or owned.stat().st_uid != os.getuid() or not owned.is_dir():
        raise ValueError("Darwin executable destination lacks an owned resolved .noindex root")
    return {"path": str(path), "ownedRoot": str(owned), "uid": os.getuid(), "spotlightExclusion": ".noindex path suffix"}


def observation():
    if platform.system() == "Linux":
        return {"platform": "Linux", "memoryPressure": Path("/proc/pressure/memory").read_text()[:1024],
                "memoryAvailable": next(line for line in Path("/proc/meminfo").read_text().splitlines() if line.startswith("MemAvailable:"))}
    if platform.system() != "Darwin":
        return {"platform": platform.system()}
    output = subprocess.check_output(["ps", "-ax", "-o", "comm="], text=True, timeout=5)
    return {"indexingProcesses": sum("mdworker" in name or "mds_stores" in name for name in output.splitlines()),
            "memoryPressure": subprocess.check_output(["memory_pressure", "-Q"], text=True, timeout=5)[-4096:]}


if __name__ == "__main__":
    print(json.dumps(require(sys.argv[1]) | {"observation": observation()}))
