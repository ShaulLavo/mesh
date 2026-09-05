#!/usr/bin/env python3
"""Isolated, interactive native provider probe. Never reads transcript files."""

import argparse
import datetime
import json
import os
from pathlib import Path
import selectors
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def append_json(path, value):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(descriptor, (json.dumps(value) + "\n").encode())
    finally:
        os.close(descriptor)


def environment(root, token=None):
    result = {"PATH": os.environ["PATH"], "HOME": str(root / "home"),
              "CODEX_HOME": str(root / "codex"), "CLAUDE_CONFIG_DIR": str(root / "claude"),
              "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache"),
              "XDG_DATA_HOME": str(root / "data"), "TMPDIR": str(root / "tmp"),
              "LANG": "en_US.UTF-8", "TERM": os.environ.get("TERM", "xterm-256color"),
              "DISABLE_AUTOUPDATER": "1"}
    if token:
        result["MESH_AGENT_TOKEN"] = token
    return result


def provider_binary(root, provider):
    executable = shutil.which(provider)
    if not executable:
        raise ValueError(provider + " is not installed")
    executable = str(Path(executable).resolve())
    version = subprocess.check_output([executable, "--version"], env=environment(root),
                                      text=True, timeout=15).strip()
    return executable, version


def link_login(root):
    codex_root = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
    claude_root = Path(os.environ.get("CLAUDE_CONFIG_DIR", str(Path.home() / ".claude")))
    for source, target in ((codex_root / "auth.json", root / "codex/auth.json"),
                           (claude_root / ".credentials.json", root / "claude/.credentials.json")):
        if source.is_file():
            target.symlink_to(source.resolve())


def prepare(args):
    parent = args.parent.expanduser().resolve()
    if parent.is_relative_to("/work") and not os.path.ismount("/work"):
        raise ValueError("/work is not mounted")
    parent.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="mesh-agent-native-", dir=parent))
    for name in ("home", "codex", "claude", "config", "cache", "data", "tmp", "project"):
        (root / name).mkdir()
    helper = Path(__file__).resolve()
    for provider in ("codex", "claude"):
        command = shlex.join([sys.executable, str(helper), "capture", str(root), provider])
        handlers = [{"hooks": [{"type": "command", "command": command}]}]
        filename = "hooks.json" if provider == "codex" else "settings.json"
        write_json(root / provider / filename,
                   {"hooks": {"SessionStart": handlers, "SessionEnd": handlers}})
    if args.use_existing_login:
        link_login(root)
    write_json(root / "setup.json", {"created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                                    "login_symlinks_requested": args.use_existing_login,
                                    "evidence": "native_probe_setup_only"})
    print(root)


def capture(args):
    raw = sys.stdin.buffer.read(32769)
    if len(raw) > 32768:
        return
    event = json.loads(raw)
    allowed = ("hook_event_name", "session_id", "cwd", "source", "agent_id")
    clean = {key: event[key] for key in allowed if key in event}
    clean.update(provider=args.provider, token=os.environ.get("MESH_AGENT_TOKEN"),
                 pid=os.getpid(), parent_pid=os.getppid())
    append_json(args.root / "events.jsonl", clean)


def launch(args):
    root = args.root.resolve()
    executable, version = provider_binary(root, args.provider)
    argv = [executable]
    if args.provider == "codex":
        argv += ["--no-alt-screen"]
    if args.shared:
        argv += ["--remote", "unix://" + str(root / "codex.sock")]
    if args.resume and args.provider == "codex":
        argv += ["resume", "--cd", str(root / "project"), "--", args.resume]
    if args.resume and args.provider == "claude":
        argv += ["--resume", args.resume]
    append_json(root / "launches.jsonl", {"provider": args.provider, "version": version,
                "executable": executable, "token": args.token, "argv": argv,
                "cwd": str(root / "project"), "shared": args.shared,
                "resume_id": args.resume, "new_prompt_in_argv": False})
    os.chdir(root / "project")
    os.execve(executable, argv, environment(root, args.token))


def daemon(args):
    root = args.root.resolve()
    executable, version = provider_binary(root, "codex")
    argv = [executable, "app-server", "--listen", "unix://" + str(root / "codex.sock")]
    append_json(root / "launches.jsonl", {"provider": "codex", "version": version,
                "executable": executable, "token": args.token, "argv": argv,
                "mode": "preexisting_native_app_server"})
    os.chdir(root / "project")
    os.execve(executable, argv, environment(root, args.token))


def response(process, expected):
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    try:
        return receive_response(process, selector, expected)
    finally:
        selector.close()


def receive_response(process, selector, expected):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        value = read_response(process, selector, expected)
        if value is not None:
            return value
    raise ValueError("native app-server request timed out")


def read_response(process, selector, expected):
    if not selector.select(1):
        return None
    raw = process.stdout.readline(2 * 1024 * 1024)
    if not raw or len(raw) >= 2 * 1024 * 1024:
        raise ValueError("native app-server closed or response exceeded limit")
    value = json.loads(raw)
    return value if value.get("id") == expected else None


def request(process, message):
    process.stdin.write(json.dumps(message) + "\n")
    process.stdin.flush()


def read_codex(args):
    root = args.root.resolve()
    executable, version = provider_binary(root, "codex")
    process = subprocess.Popen([executable, "app-server", "--stdio"], env=environment(root),
                               cwd=root / "project", stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    try:
        request(process, {"id": 1, "method": "initialize", "params": {
            "clientInfo": {"name": "mesh-native-probe", "version": "1"},
            "capabilities": {"experimentalApi": True}}})
        if "error" in response(process, 1):
            raise ValueError("native initialize failed")
        request(process, {"method": "initialized"})
        request(process, {"id": 2, "method": "thread/read", "params": {
            "threadId": args.session_id, "includeTurns": False}})
        value = response(process, 2)
        if "error" in value:
            raise ValueError("native exact-ID lookup failed")
        thread = value["result"]["thread"]
        clean = {"requested_id": args.session_id, "id": thread["id"], "cwd": thread["cwd"],
                 "version": version, "turns_returned": len(thread.get("turns", []))}
        append_json(root / "read-only-lookups.jsonl", clean)
        print(json.dumps(clean))
    finally:
        process.terminate()
        process.wait(timeout=5)


def events_for(root, provider, token):
    events = [json.loads(line) for line in (root / "events.jsonl").read_text().splitlines()]
    return [event for event in events if event.get("provider") == provider
            and event.get("token") == token and event.get("hook_event_name") == "SessionStart"]


def report(args):
    original = events_for(args.root, args.provider, args.start_token)
    resumed = events_for(args.root, args.provider, args.resume_token)
    original_ids = {event["session_id"] for event in original}
    resumed_ids = {event["session_id"] for event in resumed}
    original_cwds = {event["cwd"] for event in original}
    resumed_cwds = {event["cwd"] for event in resumed}
    resume_sources = {event.get("source") for event in resumed}
    matched = (len(original_ids) == 1 and original_ids == resumed_ids
               and original_cwds == resumed_cwds and resume_sources == {"resume"})
    result = {"provider": args.provider, "start_token": args.start_token,
              "resume_token": args.resume_token, "start_ids": sorted(original_ids),
              "resume_ids": sorted(resumed_ids), "exact_resume_hook_identity": matched,
              "evidence": "observed_native_hook_events",
              "visible_conversation": "requires_manual_UI_check",
              "pending_work": "unverified", "mesh_worker_and_picker": "not_tested_by_this_probe"}
    append_json(args.root / "native-reports.jsonl", result)
    print(json.dumps(result, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    setup = commands.add_parser("prepare", help="create isolated roots and stable hooks")
    setup.add_argument("parent", type=Path, nargs="?", default=Path("/work/tmp"))
    setup.add_argument("--use-existing-login", action="store_true", help="symlink only existing credential files")
    setup.set_defaults(run=prepare)
    for name, handler in (("capture", capture), ("launch", launch), ("daemon", daemon),
                          ("read-codex", read_codex), ("report", report)):
        child = commands.add_parser(name)
        child.add_argument("root", type=Path)
        child.set_defaults(run=handler)
    for name in ("capture", "launch", "report"):
        commands.choices[name].add_argument("provider", choices=("codex", "claude"))
    commands.choices["launch"].add_argument("token")
    commands.choices["launch"].add_argument("--resume")
    commands.choices["launch"].add_argument("--shared", action="store_true", help="Codex only: attach to preexisting server")
    commands.choices["daemon"].add_argument("token")
    commands.choices["read-codex"].add_argument("session_id")
    commands.choices["report"].add_argument("start_token")
    commands.choices["report"].add_argument("resume_token")
    args = parser.parse_args()
    if args.command == "launch" and args.shared and args.provider != "codex":
        parser.error("--shared is only supported for codex")
    os.umask(0o077)
    args.run(args)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        if "capture" not in sys.argv[1:2]:
            sys.exit(str(error))
