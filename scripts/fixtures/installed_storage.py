"""Read bounded remote ZIP metadata before acquiring large native artifacts."""
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import urllib.error
import urllib.request
import zipfile

from fixtures import installed_paths


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_arguments):
        return None


def archive_url(repository, artifact):
    request = urllib.request.Request(f"https://api.github.com/repos/{repository}/actions/artifacts/{artifact}/zip",
        headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"], "Accept": "application/vnd.github+json"})
    try:
        urllib.request.build_opener(NoRedirect).open(request, timeout=30).close()
    except urllib.error.HTTPError as error:
        if error.code == 302 and error.headers.get("Location"):
            return error.headers["Location"]
    except Exception:
        raise RuntimeError("could not obtain native artifact ZIP metadata location") from None
    raise RuntimeError("native artifact API did not return its archive location")


class RemoteZIP(io.RawIOBase):
    def __init__(self, url, length):
        self.url, self.length, self.position, self.transferred = url, length, 0, 0

    def seekable(self):
        return True

    def tell(self):
        return self.position

    def seek(self, offset, whence=0):
        bases = {0: 0, 1: self.position, 2: self.length}
        position = bases[whence] + offset
        if not 0 <= position <= self.length:
            raise ValueError("native ZIP metadata seek exceeds the artifact")
        self.position = position
        return position

    def read(self, size=-1):
        size = self.length - self.position if size < 0 else min(size, self.length - self.position)
        if not size:
            return b""
        if size > 8 << 20 or self.transferred + size > 16 << 20:
            raise RuntimeError("native ZIP directory exceeds the bounded metadata-only preflight")
        end = self.position + size - 1
        request = urllib.request.Request(self.url, headers={"Range": f"bytes={self.position}-{end}"})
        try:
            data = self.fetch(request, size, end)
        except Exception:
            raise RuntimeError("native artifact server did not supply bounded ZIP metadata ranges") from None
        self.position += size
        self.transferred += size
        return data

    def fetch(self, request, size, end):
        with urllib.request.urlopen(request, timeout=30) as response:
            expected = f"bytes {self.position}-{end}/{self.length}"
            if response.status != 206 or response.headers.get("Content-Range") != expected:
                raise ValueError("archive range differs")
            data = response.read(size + 1)
        if len(data) != size:
            raise ValueError("archive range length differs")
        return data


def preflight(repository, pin_path, key, destination, output):
    pin = json.loads(Path(pin_path).read_text())
    artifact = pin["artifacts"][key]
    directory = Path(destination).resolve()
    installed_paths.require_mounted_drive(directory)
    directory.mkdir(parents=True, exist_ok=True)
    archive = RemoteZIP(archive_url(repository, artifact["id"]), artifact["size"])
    with zipfile.ZipFile(archive) as package:
        sizes = [member.file_size for member in package.infolist() if not member.is_dir()]
    unpacked = sum(sizes)
    if not sizes or len(sizes) > 100000 or unpacked > 256 << 30:
        raise RuntimeError("native artifact ZIP inventory exceeds its input bound")
    reserve = 4 << 30
    required = artifact["size"] + unpacked + reserve
    free = shutil.disk_usage(directory).free
    short = Path(os.environ.get("MESH_SHORT_TMP", "/tmp")).resolve(strict=True)
    same_device = short.stat().st_dev == directory.stat().st_dev
    scratch_free = shutil.disk_usage(short).free
    record = {"artifact": artifact, "files": len(sizes), "unpackedBytes": unpacked, "metadataBytes": archive.transferred,
        "destination": str(directory), "freeBytes": free, "requiredBytes": required, "reserveBytes": reserve,
        "shortScratch": str(short), "sameDevice": same_device, "scratchFreeBytes": scratch_free,
        "filesystem": subprocess.check_output(["df", "-Pk", str(directory)], text=True)}
    Path(output).write_text(json.dumps(record, indent=2) + "\n")
    if free < required or not same_device and scratch_free < unpacked + reserve:
        raise RuntimeError("native runner lacks measured space for archive, extraction and owned scratch")


if __name__ == "__main__":
    import sys
    preflight(*sys.argv[1:])
