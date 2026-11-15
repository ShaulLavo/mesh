"""Attachment scopes for historical adoption and strict current update hops."""
import hashlib
import json
import shutil
import time

from fixtures import installed_clock

RECORD = "attachment-continuity.json"


def save(case, observation):
    previous = case.attachment_observations[-1] if case.attachment_observations else None
    if previous and outcome(previous) == outcome(observation):
        previous["checks"] += 1
        previous["elapsedSeconds"] += observation.get("elapsedSeconds", 0)
        observation = previous
    else:
        observation = observation | {"checks": 1}
        case.attachment_observations.append(observation)
    (case.root / RECORD).write_text(json.dumps(case.attachment_observations, indent=2) + "\n")
    return observation


def outcome(observation):
    return {key: value for key, value in observation.items() if key not in ("checks", "elapsedSeconds")}


def seed(case, *, legacy):
    from fixtures import installed_native
    image = installed_native.native_services.mapped_image(case.terminal.pid, case.binary)
    expected = case.pair["daemon"]["artifact"]["binarySha256"]
    if image["sha256"] != expected:
        raise RuntimeError("seed client differs from its actual source image")
    source = case.control("host.info")["host"]["build"]
    if not legacy and source["digest"] != expected:
        raise RuntimeError("strict current source differs from its built release")
    case.attachment = {"scope": "legacy-adoption" if legacy else "current-to-current",
                       "client": image, "session": case.session,
                       "sourceDaemon": source}
    case.attachment_terminal = case.terminal
    if not legacy:
        case.attachment["sourceDaemonImage"] = current_daemon_image(case, source)


def inspect(case):
    attachment = case.attachment
    if case.terminal is not case.attachment_terminal or case.terminal.pid != attachment["client"]["pid"]:
        raise RuntimeError("retained attachment client was replaced")
    started = time.monotonic()
    if attachment["scope"] == "legacy-adoption":
        result = inspect_legacy(case)
    else:
        result = inspect_current(case)
    return save(case, attachment | result | {"elapsedSeconds": time.monotonic() - started})


def inspect_legacy(case):
    error = None
    try:
        if case.terminal.poll() is None:
            case.wait_attachment_resume()
            case.io()
    except RuntimeError as failure:
        if case.terminal.poll() is None:
            raise
        error = str(failure)
    status = case.terminal.poll()
    return {"strictContinuityPassed": None, "exitStatus": status,
            "observedResume": status is None, "ownerReconnectRequired": status is not None,
            "resumeError": error, "reconnectAction": "owner reconnects with the current client" if status is not None else None}


def inspect_current(case):
    if case.terminal.poll() is not None:
        raise RuntimeError("strict current attachment client exited before resume")
    running = case.control("host.info")["host"]["build"]
    image = current_daemon_image(case, running)
    case.wait_attachment_resume()
    case.io()
    if case.terminal.poll() is not None:
        raise RuntimeError("strict current attachment client exited after resume")
    return {"strictContinuityPassed": True, "exitStatus": None, "ownerReconnectRequired": False,
            "runningDaemon": running, "daemonImage": image}


def current_daemon_image(case, running):
    from fixtures import installed_native
    expected = case.releases.builds[running["version"]]
    if running["digest"] != expected["digest"]:
        raise RuntimeError("strict current daemon differs from its verified release image")
    daemon = installed_native.peer_pid(case.state / "daemon.sock")
    image = installed_native.native_services.mapped_image(daemon, case.binary)
    if image["sha256"] != expected["digest"]:
        raise RuntimeError("strict current daemon maps another image")
    return image


def reconnect_current(case, version):
    """Explicit fixture action after legacy adoption, before any current hop."""
    from fixtures import installed_native, installed_transport_lifecycle
    if case.attachment["scope"] != "legacy-adoption":
        raise RuntimeError("cannot replace a client during strict current continuity")
    # Diagnostic runs retain their historical original-client assertion and fail
    # on its loss. They cannot claim a new client's socket as the old client's.
    if installed_transport_lifecycle.enabled():
        raise installed_transport_lifecycle.CoverageIncomplete("legacy diagnostic cannot claim current-client continuity")
    build = case.releases.builds[version]
    if case.control("host.info")["host"]["build"]["digest"] != build["digest"]:
        raise RuntimeError("explicit reconnect requires the adopted current daemon")
    original = case.attachment
    status = case.terminal.poll()
    if status is None:
        case.terminal.send(b"\x1d")
        case.terminal.expect_exit(timeout=installed_clock.timeout())
        if case.terminal.poll() != 0:
            raise RuntimeError("explicit legacy detach failed")
    case.terminal.close()
    case.terminal = None
    client = case.root / "current-client"
    shutil.copy2(build["path"], client)
    with client.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    if digest != build["digest"]:
        raise RuntimeError("explicit reconnect client differs from current source build")
    case.terminal = installed_native.terminal_module.Terminal([str(client), case.session],
        case.client_environment, case.root, timeout=installed_clock.timeout(), stderr_path=case.root / "current-client.err")
    case.attachment_terminal = case.terminal
    # Fresh I/O also waits for the PTY child to exec before inspecting its image.
    case.io()
    image = installed_native.native_services.mapped_image(case.terminal.pid, client)
    if image["sha256"] != build["digest"]:
        raise RuntimeError("explicit reconnect maps another current client image")
    case.attachment = {"scope": "current-to-current", "client": image, "session": case.session,
                       "sourceDaemon": build}
    save(case, {"scope": "explicit-reconnect-after-legacy-adoption", "previousClient": original["client"],
                "exitStatusBeforeAction": status, "client": image, "session": case.session,
                "strictContinuityPassed": None, "command": [str(client), case.session], "elapsedSeconds": 0})
    return inspect(case)
