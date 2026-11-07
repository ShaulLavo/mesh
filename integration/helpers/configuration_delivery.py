#!/usr/bin/env python3
"""Exercise approved feed delivery and the actual running dashboard on fixtures."""

from contextlib import ExitStack, contextmanager
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading

from automatic_fleet_updates import saved_fleet
from fleet_sharing import Device
from isolation import check_environment, termination_cleanup
from terminal_window import Terminal, eventually, require


def config(device, *arguments, success=True):
    result = device.command("update", "config", *arguments, "--json", success=success)
    return result, json.loads(result.stdout) if result.stdout else None


def proposal(device, selection, destination):
    _, result = config(device, "preview", "--file", str(selection))
    destination.write_text(json.dumps(result))
    return result


def wait_saved(coordinator, change):
    def saved():
        _, status = config(coordinator, "status", change)
        return status if status["change"]["targets"][0]["state"] == "saved" else None
    status = eventually(saved, "approved configuration was not saved", timeout=8)
    human = coordinator.command("update", "config", "status")
    require("Configuration publication" in human.stdout, human.stdout)
    require(change not in human.stdout, "default configuration status exposed change ID")
    current = coordinator.command("update", "config", "status", "--json")
    require(json.loads(current.stdout)["change"]["proposal"]["id"] == change, current.stdout)
    return status


@contextmanager
def feed_server():
    class Feed(BaseHTTPRequestHandler):
        def do_GET(self):
            label = "Fixture Alpha" if self.path == "/a" else "Fixture Beta"
            body = json.dumps({
                "schemaVersion": 1, "generatedAt": "2026-11-01T00:00:00Z",
                "accounts": [{"id": label, "provider": "fixture", "label": label,
                              "plan": "test", "checkedAt": None, "lastSeenAt": None,
                              "state": "no-data", "source": "proxy-state",
                              "routing": {"mode": "single", "active": None,
                                          "lastServedAt": None}, "windows": [],
                              "cooldown": None}],
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_arguments):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Feed)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
        require(not thread.is_alive(), "fixture feed server did not join")


def set_feed(device, url):
    path = device.config / "hosts.json"
    document = json.loads(path.read_text())
    document["dashboard"] = {"theme": "rose-pine", "usageFeedURL": url}
    path.write_text(json.dumps(document))


def displayed(terminal, screen, word, absent=None):
    def match():
        output = terminal.drain()
        frame = subprocess.check_output([screen, "160", "48"], input=output, timeout=3)
        return word.encode() in frame and (absent is None or absent.encode() not in frame)
    try:
        return eventually(match, f"dashboard did not display {word!r}", timeout=8)
    except RuntimeError as error:
        frame = subprocess.check_output([screen, "160", "48"], input=terminal.drain(), timeout=3)
        raise RuntimeError(f"{error}; fixture frame: {frame.decode()}") from error


def renderer_controls(device, feed, screen):
    set_feed(device, "")
    terminal = Terminal([device.binary, "dashboard", "--wall"],
                        dict(device.environment, TERM="xterm-256color"), device.root)
    device.terminals.append(terminal)
    terminal.resize(160, 48)
    displayed(terminal, screen, "Hosts")
    set_feed(device, feed + "/a")
    displayed(terminal, screen, "Fixture Alpha")
    set_feed(device, feed + "/b")
    displayed(terminal, screen, "Fixture Beta", "Fixture Alpha")
    path = device.config / "hosts.json"
    valid = path.read_bytes()
    path.write_text('{"version":1,"hosts":[],"unexpected":true}')
    displayed(terminal, screen, "unavailable", "Fixture Beta")
    path.write_bytes(valid)
    displayed(terminal, screen, "Fixture Beta")
    set_feed(device, "")
    displayed(terminal, screen, "Hosts", "Fixture Beta")
    os.kill(terminal.pid, signal.SIGTERM)
    terminal.expect_exit(3)


def domain_controls(coordinator, recipient, selection, preview):
    _, grant = config(recipient, "allow", coordinator.id, "--usage-feed",
                      "--domain-policy", "--expected-revision", "1")
    require(grant["usageFeed"] and grant["domainPolicy"] and grant["revision"] == 2,
            "selected configuration groups did not replace the exact grant")
    path = recipient.config / "domains.json"
    original = json.loads(path.read_text())
    desired = {"primary": "new.test", "aliases": [original["primary"]] + (original.get("aliases") or [])}
    selection.write_text(json.dumps({"targets": [recipient.id], "domain": desired}))
    proposed = proposal(coordinator, selection, preview)
    config(coordinator, "apply", "--file", str(preview), "--yes", "--command-id", "3" * 32)
    saved = wait_saved(coordinator, proposed["id"])["change"]["targets"][0]
    require(saved["receipt"]["domain"]["revision"] == 1, "domain revision was not saved")
    require("daemon restart required" in saved["domainConsumerState"], str(saved))
    instance = saved["daemon"]["instanceId"]
    actual = json.loads(path.read_text())
    require(actual["primary"] == desired["primary"] and actual["aliases"] == desired["aliases"], str(actual))
    require(actual["legacyCertificateDomain"] == original["legacyCertificateDomain"], "certificate owner changed")
    coordinator.stop_daemon()
    recipient.stop_daemon()
    recipient.start()
    coordinator.start()

    def loaded():
        _, status = config(coordinator, "status", proposed["id"])
        target = status["change"]["targets"][0]
        return target if target.get("domainConsumerState", "").startswith("daemon loaded;") else None

    observed = eventually(loaded, "actual restarted daemon policy was not observed",
                          timeout=8)
    require(observed["daemon"]["instanceId"] != instance, "saved bytes posed as a restarted daemon")
    require("service HTTPS verification unknown" in observed["domainConsumerState"], str(observed))


def main(binary, screen):
    check_environment()
    with termination_cleanup(), ExitStack() as cleanup:
        root = Path(cleanup.enter_context(tempfile.TemporaryDirectory(prefix="cf-")))
        devices = []
        for directory, name in (("a", "config-one"), ("b", "config-two")):
            (root / directory).mkdir()
            device = Device(binary, root / directory, name)
            cleanup.callback(device.close)
            devices.append(device)
        coordinator, recipient = devices
        saved_fleet(devices)
        coordinator.save_connections(recipient)
        recipient.save_connections(coordinator)
        recipient.command("update", "trust", coordinator.id)
        for device in devices:
            device.start()
        selection, preview = root / "selection.json", root / "preview.json"
        selection.write_text(json.dumps({"targets": [recipient.id], "usageFeed": {"url": ""}}))
        denied, _ = config(coordinator, "preview", "--file", str(selection), success=False)
        require(denied.returncode != 0, "administrator admission became configuration consent")
        _, grant = config(recipient, "allow", coordinator.id, "--usage-feed",
                          "--expected-revision", "0")
        require(grant["usageFeed"] and grant["revision"] == 1, str(grant))
        with feed_server() as feed:
            set_feed(recipient, feed + "/a")
            proposed = proposal(coordinator, selection, preview)
            set_feed(recipient, feed + "/b")
            config(coordinator, "apply", "--file", str(preview), "--yes",
                   "--command-id", "1" * 32)
            def conflicted():
                _, status = config(coordinator, "status", proposed["id"])
                return status["change"]["targets"][0]["state"] == "conflict"
            eventually(conflicted, "selected local edit was overwritten", timeout=8)
            require(json.loads((recipient.config / "hosts.json").read_text())["dashboard"]["usageFeedURL"] == feed + "/b", "selected local edit lost")
            proposed = proposal(coordinator, selection, preview)
            set_feed(recipient, feed + "/b")
            config(coordinator, "apply", "--file", str(preview), "--yes",
                   "--command-id", "2" * 32)
            saved = wait_saved(coordinator, proposed["id"])
            require(saved["change"]["targets"][0]["receipt"]["revision"] == 1, str(saved))
            for device in devices:
                device.stop_daemon()
                device.start()
            _, replayed = config(coordinator, "apply", "--file", str(preview), "--yes",
                                 "--command-id", "2" * 32)
            require(replayed["revision"] == saved["revision"], "restart replay advanced authority")
            book = json.loads((recipient.config / "hosts.json").read_text())
            require(book["dashboard"]["theme"] == "rose-pine" and book["hosts"][0]["id"] == coordinator.id, "delivery changed unrelated settings")
            renderer_controls(recipient, feed, screen)
        domain_controls(coordinator, recipient, selection, preview)
        _, revoked = config(recipient, "revoke", "--expected-revision", "2")
        require(not revoked["usageFeed"] and not revoked["domainPolicy"] and revoked["revision"] == 3, str(revoked))
        print("PASS: real signed configuration CLI, feed renderer generations, saved domain versus actual process policy; HTTPS verification remains unknown")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
