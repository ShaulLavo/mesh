"""Execute local host profiles with private state and no daemon or network."""

from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import subprocess
import sys


def invoke(binary, root, args, expected, operation=None, code=None, document=None):
    environment = dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                       MESH_STATE_DIR=str(root / "state"), MESH_PRIVACY="0",
                       VISUAL="exit 88", EDITOR="exit 89")
    result = subprocess.run([binary, *args], env=environment, input=document,
                            capture_output=True, text=True, timeout=10, check=False)
    assert result.returncode == expected, (args, result.returncode, result.stderr, result.stdout)
    if operation is None:
        assert not result.stdout, (args, result.stdout)
        return None
    value = json.loads(result.stdout)
    assert value["operation"] == operation, value
    assert value["schemaVersion"] == 1 and len(value["requestId"]) == 26, value
    assert not result.stderr, result.stderr
    if code:
        assert value["status"] == "error" and value["data"] is None, value
        assert len(value["errors"]) == 1 and value["errors"][0]["code"] == code, value
    else:
        assert value["status"] == "ok" and value["errors"] == [], value
    return value["data"]


def absent(root):
    for name in ("state", "config"):
        assert not (root / name).exists(), f"usage/read created {name}"


def early_cases(binary, root):
    cases = [(["profile", "nope", "--json"], "profile.show"),
             (["profile", "--json"], "profile.show"),
             (["profile", "show", "--json", "--bogus"], "profile.show"),
             (["profile", "show", "--json", "extra"], "profile.show"),
             (["profile", "set", "--json"], "profile.set"),
             (["profile", "edit", "--json"], "profile.edit"),
             (["profile", "show", "--json", "--", "--json=false"], "profile.show"),
             (["profile", "set", "--json", "--file", "--json=false",
               "--expected-revision", "bad"], "profile.set")]
    for args, operation in cases:
        invoke(binary, root, args, 2, operation, "usage.invalid")
        absent(root)
    invoke(binary, root, ["profile", "show", "--json"], 1,
           "profile.show", "host.uninitialized")
    absent(root)
    invoke(binary, root, ["profile", "show", "--", "--json"], 2)
    absent(root)


def local_cases(binary, root):
    environment = dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                       MESH_STATE_DIR=str(root / "state"))
    subprocess.run([binary, "device", "identity", "--json"], env=environment,
                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, check=True)
    before = {p.name: p.read_bytes() for p in (root / "state").iterdir()}
    assert set(before) == {"identity.key"}, before.keys()
    show = ["profile", "show", "--json"]
    data = invoke(binary, root, show, 0, "profile.show")
    assert data["state"] == "absent" and data["revision"] == 0, data
    assert not (root / "config").exists()
    base = ["profile", "set", "--json", "--file", "-", "--expected-revision", "0"]
    for text in ('{"schemaVersion":2}', '{"schemaVersion":1,"description":"\\u202e"}',
                 '{"schemaVersion":1,"labels":{"a":"b","A":"c"}}', ' ' * 32769):
        invoke(binary, root, base, 2, "profile.set", "profile.invalid", text)
        assert not (root / "config").exists()
    document = '{"schemaVersion":1,"description":"local notes","instructions":"one\\ntwo"}'
    first = invoke(binary, root, base, 0, "profile.set", document=document)
    assert first["revision"] == 1 and first["changed"] and first["committed"], first
    assert first["publication"] == "complete" and first["members"] == [], first
    retry = invoke(binary, root, base, 0, "profile.set", document=document)
    assert retry["revision"] == 1 and not retry["changed"] and retry["committed"], retry
    data = invoke(binary, root, show, 0, "profile.show")
    assert data["profile"]["instructions"] == "one\ntwo", data
    assert data["profile"]["labels"] == {} and data["profile"]["warnings"] == [], data
    invoke(binary, root, base, 1, "profile.set", "profile.conflict", '{"schemaVersion":1}')
    invoke(binary, root, show + ["--privacy"], 2, "profile.show", "usage.invalid")
    invoke(binary, root, show + ["--privacy", "--privacy=false"], 0, "profile.show")
    clear = base[:-1] + ["1"]
    data = invoke(binary, root, clear, 0, "profile.set", document='{"schemaVersion":1}')
    assert data["revision"] == 2 and data["changed"], data
    assert before == {p.name: p.read_bytes() for p in (root / "state").iterdir()}
    assert {p.name for p in (root / "config").iterdir()} == {"profile.json", ".profile.lock"}


def concurrent_cases(binary, root):
    for identical in (False, True):
        directory = root / ("identical" if identical else "different")
        directory.mkdir(mode=0o700)
        environment = dict(os.environ, MESH_CONFIG_DIR=str(directory / "config"),
                           MESH_STATE_DIR=str(directory / "state"), MESH_PRIVACY="0")
        subprocess.run([binary, "device", "identity", "--json"], env=environment,
                       capture_output=True, timeout=10, check=True)
        args = [binary, "profile", "set", "--file", "-", "--expected-revision", "0", "--json"]
        documents = ['{"schemaVersion":1,"instructions":"first"}',
                     '{"schemaVersion":1,"instructions":"first"}' if identical else
                     '{"schemaVersion":1,"instructions":"second"}']
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(subprocess.run, args, input=text, env=environment,
                                   capture_output=True, text=True, timeout=10, check=False)
                       for text in documents]
            results = [future.result() for future in futures]
        expected = [0, 0] if identical else [0, 1]
        assert sorted(result.returncode for result in results) == expected
        values = [json.loads(result.stdout) for result in results]
        assert all(not result.stderr for result in results)
        committed = [value["data"] for value in values if value["status"] == "ok"]
        assert sum(data["changed"] for data in committed) == 1, committed
        assert all(data["revision"] == 1 for data in committed), committed
        stored = json.loads((directory / "config" / "profile.json").read_text())
        assert stored["revision"] == 1, stored


def editor_diagnostic_case(binary, root):
    directory = root / "editor-diagnostic"
    directory.mkdir(mode=0o700)
    environment = dict(os.environ, MESH_CONFIG_DIR=str(directory / "config"),
                       MESH_STATE_DIR=str(directory / "state"), MESH_PRIVACY="0", NO_COLOR="1")
    subprocess.run([binary, "device", "identity", "--json"], env=environment,
                   capture_output=True, timeout=10, check=True)
    before = (directory / "state" / "identity.key").read_bytes()
    document = json.dumps({"schemaVersion": 1, "\u202ePROFILE_FIELD_MARKER\u202c": "unexpected"})
    editor = directory / "editor"
    editor.write_text("#!/bin/sh\nprintf '%s' '" + document + "' > \"$1\"\n")
    editor.chmod(0o700)
    environment.update(VISUAL=str(editor), EDITOR="exit 89")
    result = subprocess.run([binary, "profile", "edit"], env=environment,
                            capture_output=True, text=True, timeout=10, check=False)
    assert result.returncode == 1 and not result.stdout, (result.returncode, repr(result.stdout))
    assert "PROFILE_FIELD_MARKER" in result.stderr and "editable copy kept at" in result.stderr, repr(result.stderr)
    assert "\u202e" not in result.stderr and "\u202c" not in result.stderr, repr(result.stderr)
    assert not (directory / "config").exists(), "invalid editor document created profile state"
    retained = list((directory / "state").glob(".profile-*.json"))
    assert len(retained) == 1 and retained[0].read_text() == document, retained
    assert (directory / "state" / "identity.key").read_bytes() == before


def main():
    binary, directory = sys.argv[1:]
    root = Path(directory).resolve()
    early_cases(binary, root)
    local_cases(binary, root)
    concurrent_cases(binary, root)
    editor_diagnostic_case(binary, root)
    print("PASS: profile group/leaf errors, explicit stdin, stopped-daemon reads/writes and noncreation")


if __name__ == "__main__":
    main()
