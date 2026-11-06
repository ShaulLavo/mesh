#!/usr/bin/env python3
"""Build each Mesh variant once, then run integrations with bounded concurrency.

verify.sh execs this controller before creating anything. Setup, both builds
and every integration run as owned commands: a Linux subreaper owner joins
every process the job starts before deleting the job's scratch root. This
process never signals a job. It records the first termination signal,
publishes it as the run's cancel file, and exits with that signal's status.
The run root is deleted only after every job is joined or declined and its
owner and launcher exits are proved; otherwise it is kept and reported.
"""
import glob
import json
import locale
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import traceback

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration" / "helpers"))

from isolation import refuse_live_paths  # noqa: E402 - helpers are found through the path above
from owned_command import DEFAULTS, Controller, Trigger, Triggers  # noqa: E402
from process_tree import Kernel, Unsupported  # noqa: E402

# Packaging builds release archives; name observation crosses real freshness windows.
# Large app transfers also sync hundreds of chunks to disk. These need the
# slow budget independently of ordinary session tests.
SLOW_TESTS = frozenset({"packaging_contract.sh", "name_observation.sh", "temporary_app_size.sh", "copy_files.sh"})
GO_PATHS = ("GOROOT", "GOCACHE", "GOMODCACHE")
# Module downloads need the caller's network policy; loopback fixtures do not.
GO_POLICY = ("GOPROXY", "GONOSUMDB", "GONOPROXY", "GOPRIVATE", "GOFLAGS", "GOINSECURE", "GOSUMDB")
BUILD_NETWORK = ("HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "no_proxy",
                 "all_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR")
TEST_PASSTHROUGH = ("XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "MESH_TEST_ZSH", "MESH_SHORT_TMP")
DURATION = re.compile(r"(\d+(?:\.\d*)?|\.\d+)([smhd]?)")
UNITS = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}
# Test only: a directory of holds, faults and shorter stage timings for the runner regression.
CONTROL = "MESH_INTEGRATION_CONTROL"
UNBOUNDED = float("inf")


class Refused(Exception):
    """A configuration the runner refuses before creating anything."""


def duration(name, value):
    match = DURATION.fullmatch(value)
    if not match or float(match[1]) <= 0:
        raise Refused(f"{name} must be a positive duration such as 30s")
    return float(match[1]) * UNITS[match[2]]


def budgets(environ):
    return {"normal": duration("MESH_INTEGRATION_TIMEOUT", environ.get("MESH_INTEGRATION_TIMEOUT") or "30s"),
            "slow": duration("MESH_INTEGRATION_SLOW_TIMEOUT", environ.get("MESH_INTEGRATION_SLOW_TIMEOUT") or "600s")}


def budget_for(name, budget):
    return budget["slow"] if name in SLOW_TESTS else budget["normal"]


def job_count(environ):
    value = environ.get("MESH_INTEGRATION_JOBS") or ""
    if not value:
        available = len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else os.cpu_count()
        return min(available or 2, 4)
    if not re.fullmatch(r"[1-9][0-9]?", value) or int(value) > 64:
        raise Refused("MESH_INTEGRATION_JOBS must be between 1 and 64")
    return int(value)


def discover(repo):
    """Integration entries in the shell's glob order."""
    try:
        locale.setlocale(locale.LC_COLLATE, "")
    except locale.Error:
        pass
    names = sorted(glob.glob("*.sh", root_dir=repo / "integration"), key=locale.strxfrm)
    return [repo / "integration" / name for name in names]


def load_control(environ):
    """Per-job control directories and shorter stage timings, only when the regression asks for them."""
    control = environ.pop(CONTROL, None)
    if not control:
        return None, {}
    path = Path(control) / "settings.json"
    settings = json.loads(path.read_text()) if path.exists() else {}
    return Path(control), {name: float(value) for name, value in settings.items() if name in DEFAULTS and name != "budget"}


def caller_user_bus():
    try:
        system = subprocess.run(["uname", "-s"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        if system.stdout.strip() != "Linux":
            return False
        return subprocess.run(["busctl", "--user", "status"], stdout=subprocess.DEVNULL,
                              stderr=subprocess.DEVNULL).returncode == 0
    except OSError:
        return False


def report_setup(repo):
    """The setup job's leader: read the caller's Go settings and bus once, inside ownership."""
    values = {}
    for name in GO_PATHS + GO_POLICY:
        try:
            result = subprocess.run(["go", "env", name], cwd=repo, stdout=subprocess.PIPE, text=True)
        except OSError as error:
            print(f"FAIL: go env {name}: {error}", file=sys.stderr)
            return 1
        if result.returncode:
            print(f"FAIL: go env {name} exited with status {result.returncode}", file=sys.stderr)
            return 1
        values[name] = result.stdout.rstrip("\n")
    values["user_bus"] = caller_user_bus()
    print(json.dumps(values))
    return 0


def copy_log(path, leaks_only=False):
    try:
        text = Path(path).read_text(errors="replace")
    except OSError:
        return
    lines = [line for line in text.splitlines() if line.startswith("LEAK:")] if leaks_only else text.splitlines()
    for line in lines:
        print(line, file=sys.stderr)
    sys.stderr.flush()


class Run:
    def __init__(self, repo, bash, environ, jobs, budget, controller, control, tmpdir):
        self.repo = repo
        self.bash = bash
        self.caller = environ
        self.jobs = jobs
        self.budget = budget
        self.controller = controller
        self.control = control
        self.tmpdir = tmpdir
        self.root = controller.root
        self.tools = self.root / "bin"
        self.binary = self.root / "mesh"
        self.integration_binary = self.root / "mesh-integration"
        self.reported = set()
        self.printed = []
        self.setup = None
        self.test_env = None
        self.build_env = None

    def start(self, name, output, error, environment, **job):
        if self.control is not None:
            job["control"] = str(self.control / name)
        popen = {"stdout": output, "stderr": error, "stdin": subprocess.DEVNULL, "cwd": self.repo, "env": environment}
        return self.controller.start(name, popen, cwd=str(self.repo), inherit_env=True, **job)

    def execute(self):
        if not self.read_setup():
            return 1
        self.prepare_environments()
        for name, flags, failure in (("build", [], "FAIL: build"),
                                     ("integration-build", ["-tags", "mesh_integration"], "FAIL: integration build")):
            if not self.build(name, flags, failure):
                return 1
        print(f"Running integration tests with {self.jobs} concurrent jobs", flush=True)
        tests = discover(self.repo)
        if not tests:
            print("FAIL: no integration scripts found", file=sys.stderr, flush=True)
            return 1
        return self.run_tests(tests)

    def read_setup(self):
        result = self.root / "setup.json"
        log = self.root / "setup.log"
        command = [sys.executable, "-B", str(Path(__file__).resolve()), "--setup", str(self.repo)]
        with open(result, "w") as output, open(log, "w") as error:
            job = self.start("setup", output, error, self.caller, command=command, budget=UNBOUNDED)
        if not self.controller.wait([job], until_cancelled=True):
            return False
        copy_log(log)
        if not self.succeeded(job):
            self.report_state(job)
            return False
        self.setup = json.loads(result.read_text())
        return True

    def prepare_environments(self):
        caller, setup = self.caller, self.setup
        # Version-manager shims depend on HOME. Pin the interpreters before replacing
        # it, and reuse compiler caches so isolation does not turn warm builds cold.
        self.tools.mkdir()
        for name, target in (("python3", os.path.realpath(sys.executable)), ("bash", self.bash), ("sh", "/bin/sh")):
            (self.tools / name).symlink_to(target)
        self.test_env = {
            "PATH": f"{self.tools}:{setup['GOROOT']}/bin:{caller.get('PATH', '')}", "TMPDIR": self.tmpdir,
            "TERM": caller.get("TERM") or "dumb", "LANG": caller.get("LANG") or "C",
            "GOCACHE": setup["GOCACHE"], "GOMODCACHE": setup["GOMODCACHE"], "PYTHONDONTWRITEBYTECODE": "1",
        }
        self.test_env.update({name: caller[name] for name in TEST_PASSTHROUGH if name in caller})
        build = dict(self.test_env)
        # An exported empty value is still an explicit caller override.
        build.update({name: caller.get(name, setup[name]) for name in GO_POLICY})
        build.update({name: caller[name] for name in BUILD_NETWORK if name in caller})
        netrc = caller.get("NETRC") or caller.get("HOME", "") + "/.netrc"
        if caller.get("NETRC") or os.path.isfile(netrc):
            build["NETRC"] = os.path.normpath(os.path.join(os.path.realpath(self.repo), netrc))
        home = self.root / "build-home"
        home.mkdir()
        self.build_env = {**build, "HOME": str(home)}

    def build(self, name, flags, failure):
        log = self.root / f"{name}.log"
        target = self.integration_binary if flags else self.binary
        with open(log, "w") as output:
            job = self.start(name, output, subprocess.STDOUT, self.build_env, budget=UNBOUNDED,
                             command=["go", "build", *flags, "-o", str(target), "./cmd/mesh"])
        if not self.controller.wait([job], until_cancelled=True):
            return False
        copy_log(log)
        if self.succeeded(job):
            return True
        print(failure, file=sys.stderr, flush=True)
        self.report_state(job)
        return False

    def run_tests(self, tests):
        launched = []
        for path in tests:
            if not self.report_until_free(launched, self.jobs - 1) or self.controller.check() is not None:
                return 1
            launched.append(self.start_test(path))
        if not self.report_until_free(launched, 0):
            return 1
        return 1 if any(not self.succeeded(job) for job in launched) else 0

    def report_until_free(self, launched, outstanding):
        """Report jobs in launch order until at most outstanding remain; False when cancelled first."""
        while len(launched) - len(self.printed) > outstanding:
            if not self.report_next(launched[len(self.printed)]):
                return False
            self.printed.append(launched[len(self.printed)])
        return True

    def start_test(self, path):
        name = path.name
        config = self.root / "config" / name
        home = self.root / "home" / name
        config.mkdir(parents=True)
        home.mkdir(parents=True)
        # Provider routing, proxies and shell startup files must come from fixtures,
        # not the developer's environment.
        environment = {**self.test_env, "HOME": str(home), "MESH": str(self.binary),
                       "MESH_INTEGRATION_BINARY": str(self.integration_binary), "MESH_CONFIG_DIR": str(config)}
        bash = str(self.tools / "bash")
        isolation = {"bash": bash, "script": str(path), "user_bus": name == "session_scope.sh" and self.setup["user_bus"]}
        with open(self.root / f"{name}.log", "w") as output:
            return self.start(name, output, subprocess.STDOUT, environment, command=[bash, str(path)],
                              isolation=isolation, budget=budget_for(name, self.budget), scratch_parent=self.tmpdir)

    def report_next(self, job):
        if not self.controller.wait([job], until_cancelled=True):
            return False
        log = self.root / f"{job.name}.log"
        if self.succeeded(job):
            print(f"PASS: {job.name}", flush=True)
            copy_log(log, leaks_only=True)
            return True
        code = job.outcome.get("returncode")
        print(f"FAIL: {job.name} (exit {'unknown' if code is None else code})", file=sys.stderr)
        for line in log.read_text(errors="replace").splitlines():
            print("  " + line, file=sys.stderr)
        sys.stderr.flush()
        self.report_state(job)
        return True

    @staticmethod
    def succeeded(job):
        return job.state() == "joined" and job.outcome.get("returncode") == 0

    def report_state(self, job):
        if job.proved() or job.name in self.reported:
            return
        self.reported.add(job.name)
        print(f"FAIL: {job.name}: {job.state()}: {'; '.join(job.outcome.get('reasons', []))}",
              file=sys.stderr, flush=True)

    def finish(self, status):
        """Wait, within each job's deadline, for every launched job; then delete or keep the run root."""
        controller = self.controller
        live = [job for job in controller.jobs if job.outcome is None]
        if live and controller.cancelled is None:
            controller.cancel(Trigger(signal.SIGTERM, 1, "controller stopped", controller.clock()))
        controller.wait(live)
        if controller.triggers.first is not None:
            # The first signal decides the status; later ones change nothing.
            status = controller.triggers.first.status
        for job in controller.jobs:
            self.report_state(job)
        for failure in controller.failures:
            print(f"FAIL: {failure}", file=sys.stderr, flush=True)
        if not controller.proved():
            print(f"FAIL: kept {self.root}", file=sys.stderr, flush=True)
            return status or 1
        try:
            shutil.rmtree(self.root)
        except OSError as error:
            print(f"FAIL: remove {self.root}: {error}", file=sys.stderr, flush=True)
            return status or 1
        return status


def main(arguments):
    if arguments[:1] == ["--setup"]:
        return report_setup(Path(arguments[1]))
    repo, bash = Path(arguments[0]), arguments[1]
    environ = dict(os.environ)
    tmpdir = environ.get("TMPDIR") or "/tmp"
    try:
        jobs = job_count(environ)
        budget = budgets(environ)
        refuse_live_paths({"TMPDIR": tmpdir, "MESH_SHORT_TMP": environ.get("MESH_SHORT_TMP")}, parents=False)
        # Refuse before creating anything where children cannot be joined.
        Kernel().require()
        control, overrides = load_control(environ)
    except (Refused, Unsupported, RuntimeError, OSError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    triggers = Triggers()
    root = Path(tempfile.mkdtemp(prefix="mesh-verify.", dir=tmpdir))
    controller = Controller(root, triggers, overrides, control and control / "runner")
    run = Run(repo, bash, environ, jobs, budget, controller, control, tmpdir)
    try:
        status = run.execute()
    except Exception:
        # Keep going to finish: live jobs must still be cancelled and joined before deletion.
        traceback.print_exc()
        status = 1
    return run.finish(status)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
