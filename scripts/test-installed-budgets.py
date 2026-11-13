"""Pure frozen selection and workflow admission checks; no native fixture runs."""
from dataclasses import asdict
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from fixtures import installed_budgets as budgets, installed_clock as clocks

ROOT = Path(__file__).resolve().parent.parent
WORKFLOW = (ROOT / ".github/workflows/installed-catchup.yml").read_text()
SMOKE = (ROOT / "scripts/fixtures/installed-frozen-smoke.yml").read_text()


def job(name):
    return re.search(rf"^  {name}:\n(.*?)(?=^  [\w-]+:|\Z)", WORKFLOW, re.M | re.S).group(1)


def synthetic_budget(key):
    workload = {"operation": key}
    sample = clocks.SampleRef({"testOnly": True}, workload, clocks.SECOND, True, True)
    return clocks.PhaseBudget(key, clocks.bound(clocks.SECOND), workload, (sample,))


class FrozenBudgetTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="frozen-wiring-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.index = self.root / "index.json"
        self.args = SimpleNamespace(source_host="pc", scenario=None, worker_registry=self.root / "workers.json",
            clock_mode="frozen", clock_plan=None, clock_plan_index=self.index, clock_job_start=self.root / "origin.json",
            fresh_managed=False, ordered_hosts=False, format_waypoint=False, signed_chain=False)
        self.args.worker_registry.write_text('{"testOnly":true}')
        self.origin = clocks.time.monotonic_ns()
        self.args.clock_job_start.write_text(json.dumps({"origin_ns": self.origin, "run": "test-run", "job": "test-job",
                                                       "admission_ns": 1200 * clocks.SECOND}))
        self.environment = patch.dict(os.environ, GITHUB_RUN_ID="test-run", GITHUB_JOB="test-job", MESH_CATALOG_EVIDENCE="0")
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def write_plan(self, platform="linux/amd64", mode="oldest", scenario=None, catalog=False, **changes):
        key, context = budgets.selection(platform, mode, scenario, catalog)
        workload = context | {"publishedRegistrySha256": hashlib.sha256(self.args.worker_registry.read_bytes()).hexdigest()
                              if self.args.worker_registry else None,
                              "buildRecipe": "mesh_installed_fixture/all-three-platforms/startup/full-transition",
                              "manifestSchemas": [1, 2] if mode == "waypoint" else [1]}
        phase, external = synthetic_budget("setup#1"), synthetic_budget("external")
        plan = clocks.ClockPlan(mode="frozen", workload=workload, phases=(phase,), command_ns=phase.limit_ns,
            external_ns=external.limit_ns, emergency_ns=clocks.SECOND, eligibility_review="test-only approval",
            external_budget=external, admission_ns=1175 * clocks.SECOND)
        value = asdict(plan) | changes
        (self.root / (key + ".json")).write_text(json.dumps(value))
        entries = json.loads(self.index.read_text())["plans"] if self.index.exists() else {}
        entries[key] = {"path": key + ".json"}
        self.index.write_text(json.dumps({"schema": 1, "plans": entries}))
        return key, context

    def test_missing_approval_fails_before_fixture_dispatch(self):
        with self.assertRaisesRegex(ValueError, "inactive.*index is absent.*activated explicitly"):
            budgets.bind(self.args, "linux/amd64")

    def test_missing_cell_and_missing_plan_fail_clearly(self):
        self.index.write_text('{"schema":1,"plans":{}}')
        with self.assertRaisesRegex(ValueError, "no independently approved.*linux-amd64-oldest"):
            budgets.bind(self.args, "linux/amd64")
        key, _context = self.write_plan()
        (self.root / (key + ".json")).unlink()
        with self.assertRaisesRegex(ValueError, "plan source is absent"):
            budgets.bind(self.args, "linux/amd64")

    def test_unreviewed_candidate_and_diagnostic_file_are_rejected(self):
        self.write_plan(eligibility_review="")
        with self.assertRaisesRegex(ValueError, "reviewed eligibility"):
            budgets.bind(self.args, "linux/amd64")
        key, context = self.write_plan()
        path = self.root / (key + ".json")
        value = json.loads(path.read_text())
        value.update(mode="diagnostic", phases=[], command_ns=clocks.SECOND)
        path.write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError, "selection differs"):
            budgets.select_plan(self.index, key, context)

    def test_plan_binding_preserves_original_origin_and_registry_identity(self):
        self.write_plan()
        budgets.bind(self.args, "linux/amd64")
        clock = clocks.from_arguments(self.args, "linux/amd64")
        self.assertEqual(clock.job_origin_ns, self.origin)
        self.assertEqual(clock.plan.mode, "frozen")
        self.args.worker_registry.write_text('{"testOnly":"changed"}')
        with self.assertRaisesRegex(ValueError, "workload differs from actual invocation"):
            clocks.from_arguments(self.args, "linux/amd64")

    def test_mode_host_scenario_and_catalog_are_not_interchangeable(self):
        self.write_plan()
        for field, changed in (("mode", "chain"), ("host", "pi"), ("scenario", "interrupt"), ("catalogEvidence", True)):
            key, context = self.write_plan()
            path = self.root / (key + ".json")
            value = json.loads(path.read_text())
            value["workload"][field] = changed
            path.write_text(json.dumps(value))
            with self.assertRaisesRegex(ValueError, "selection differs"):
                budgets.select_plan(self.index, key, context)

    def test_original_job_identity_and_live_setup_time_stay_enforced(self):
        self.write_plan()
        budgets.bind(self.args, "linux/amd64")
        with patch.dict(os.environ, GITHUB_JOB="another-job"), self.assertRaisesRegex(ValueError, "another job"):
            clocks.from_arguments(self.args, "linux/amd64")
        with self.assertRaisesRegex(clocks.ClockExpired, "insufficient live job admission"):
            clocks.Clock(clocks.load_plan(self.args.clock_plan), origin_ns=self.origin + 2 * clocks.SECOND,
                         job_origin_ns=self.origin)

    def test_admission_rounds_up_and_rejects_over_thirty_minutes(self):
        key, context = self.write_plan()
        _path, plan = budgets.select_plan(self.index, key, context)
        self.assertEqual(budgets.workflow_minutes(plan), 20)
        self.write_plan(admission_ns=1800 * clocks.SECOND)
        self.assertEqual(budgets.workflow_minutes(budgets.select_plan(self.index, key, context)[1]), 30)
        self.write_plan(admission_ns=1800 * clocks.SECOND + 1)
        with self.assertRaisesRegex(ValueError, "30-minute ceiling"):
            budgets.select_plan(self.index, key, context)

    def test_index_cannot_escape_fixture_directory(self):
        self.index.write_text(json.dumps({"schema": 1, "plans": {"cell": {"path": "../outside.json"}}}))
        with self.assertRaisesRegex(ValueError, "within the fixture budget directory"):
            budgets.select_plan(self.index, "cell", {})

    def test_fresh_and_standalone_profiles_are_distinct_from_grouped(self):
        self.args.worker_registry, self.args.source_host = None, None
        self.write_plan(mode="adoption")
        budgets.bind(self.args, "linux/amd64")
        self.assertEqual(clocks.from_arguments(self.args, "linux/amd64").plan.workload["mode"], "adoption")
        self.args.clock_plan, self.args.fresh_managed = None, True
        with self.assertRaisesRegex(ValueError, "linux-amd64-fresh"):
            budgets.bind(self.args, "linux/amd64")
        self.write_plan(mode="fresh")
        budgets.bind(self.args, "linux/amd64")
        self.assertEqual(clocks.from_arguments(self.args, "linux/amd64").plan.workload["mode"], "fresh")

    def test_catalog_requires_separate_linux_oldest_approval(self):
        self.write_plan()
        with patch.dict(os.environ, MESH_CATALOG_EVIDENCE="1"):
            with self.assertRaisesRegex(ValueError, "oldest-catalog"):
                budgets.bind(self.args, "linux/amd64")
            self.write_plan(catalog=True)
            budgets.bind(self.args, "linux/amd64")
            self.assertTrue(clocks.from_arguments(self.args, "linux/amd64").plan.workload["catalogEvidence"])

    def test_ordinary_oldest_catalog_selections_match_the_approved_workloads(self):
        keys = {key for key, _context in budgets.required_profiles(True, False, None, True)}
        self.assertIn("linux-amd64-oldest-catalog", keys)
        self.assertIn("linux-arm64-oldest-catalog", keys)
        self.assertIn("darwin-arm64-oldest", keys)
        self.assertNotIn("linux-amd64-oldest", keys)
        self.assertNotIn("darwin-arm64-oldest-catalog", keys)

    def test_explicit_diagnostic_does_not_read_or_populate_index(self):
        self.args.clock_mode = "diagnostic"
        with patch.object(budgets, "select_plan") as select:
            budgets.bind(self.args, "linux/amd64")
        select.assert_not_called()
        self.assertFalse(self.index.exists())
        args = SimpleNamespace(index=self.index, cohort=False, support=False, adoption_scenario=None, catalog_evidence=False)
        self.assertEqual(budgets.workflow_budgets(args), {})

    def test_prepared_roles_never_reuse_unsplit_logical_plans(self):
        self.write_plan()
        self.args.export_prepared = self.root / "export"
        with self.assertRaisesRegex(ValueError, "physical slice index mapping is pending"):
            budgets.bind(self.args, "linux/amd64")
        self.args.clock_plan = self.root / "linux-amd64-oldest.json"
        with self.assertRaisesRegex(ValueError, "prepared role differs"):
            budgets.bind(self.args, "linux/amd64")
        value = json.loads(self.args.clock_plan.read_text())
        value["workload"]["preparedRole"] = "prepare"
        self.args.clock_plan.write_text(json.dumps(value))
        budgets.bind(self.args, "linux/amd64")
        self.args.export_prepared, self.args.prepared_releases = None, self.root / "bundle"
        with self.assertRaisesRegex(ValueError, "prepared role differs"):
            budgets.bind(self.args, "linux/amd64")
        value["workload"]["preparedRole"] = "consume"
        self.args.clock_plan.write_text(json.dumps(value))
        budgets.bind(self.args, "linux/amd64")

    def test_actual_fresh_plan_admits_only_its_exact_workload(self):
        self.args.clock_plan_index = budgets.INDEX
        self.args.source_host, self.args.worker_registry, self.args.fresh_managed = None, None, True
        budgets.bind(self.args, "linux/amd64")
        plan = clocks.from_arguments(self.args, "linux/amd64").plan
        self.assertEqual(plan.workload["mode"], "fresh")
        self.assertEqual(budgets.workflow_minutes(plan), 15)
        self.assertEqual(plan.admission_ns, 900 * clocks.SECOND)
        self.assertEqual(plan.emergency_ns, 60 * clocks.SECOND)
        for field, value in (("source_host", "pc"), ("scenario", "initial")):
            setattr(self.args, field, value)
            with self.assertRaisesRegex(ValueError, "workload differs from actual invocation"):
                clocks.from_arguments(self.args, "linux/amd64")
            setattr(self.args, field, None)
        with patch.dict(os.environ, MESH_CATALOG_EVIDENCE="1"), self.assertRaisesRegex(ValueError, "workload differs from actual invocation"):
            clocks.from_arguments(self.args, "linux/amd64")

    def test_unreviewed_four_consumer_slots_stay_absent(self):
        plans = budgets.read_index(budgets.INDEX)
        self.assertEqual(len(plans), 33)
        missing = ("linux-amd64-chain-consume-final", "linux-arm64-chain-consume-none",
                   "linux-arm64-waypoint-consume-final", "darwin-arm64-chain-consume-none")
        for key in missing:
            self.assertNotIn(key, plans)
            with self.assertRaisesRegex(ValueError, "no independently approved frozen plan"):
                budgets.select_plan(budgets.INDEX, key, {})

    def test_reviewed_order_plan_preserves_its_complete_admission(self):
        key, workload = budgets.selection("linux/amd64", "order")
        _path, plan = budgets.select_plan(budgets.INDEX, key, workload)
        self.assertEqual(plan.command_ns + plan.external_ns + plan.emergency_ns, 688_905_000_000)
        self.assertEqual(plan.admission_ns, 900 * clocks.SECOND)
        self.assertEqual(budgets.workflow_minutes(plan), 15)
        self.assertEqual((len(plan.phases), len(plan.guards)), (43, 11))

    def test_all_ten_cohort_cells_and_six_support_cells_are_required(self):
        profiles = budgets.required_profiles(True, True, None, False)
        self.assertEqual(len(profiles), 16)
        self.assertEqual(len({key for key, _context in profiles}), 16)
        self.assertEqual(sum(context["grouped"] for _key, context in profiles), 10)
        self.assertIn(("linux-amd64-order", budgets.selection("linux/amd64", "order")[1]), profiles)
        for key, context in profiles:
            self.write_plan(context["platform"], context["mode"], context["scenario"], context["catalogEvidence"])
        args = SimpleNamespace(index=self.index, cohort=True, support=True, adoption_scenario=None, catalog_evidence=False)
        self.assertEqual(budgets.workflow_budgets(args), {key: 20 for key, _context in profiles})


class WorkflowWiringTests(unittest.TestCase):
    def script(self, source, step_name):
        step = source.split("      - name: " + step_name + "\n", 1)[1].split("      - ", 1)[0]
        return "\n".join(line[10:] for line in step.split("        run: |\n", 1)[1].splitlines())

    def test_frozen_smoke_reuses_one_native_fresh_job_without_diagnostic_mode(self):
        self.assertFalse((ROOT / ".github/workflows/installed-frozen-smoke.yml").exists())
        self.assertTrue((ROOT / ".github/workflows/rollback-proof-investigation.yml").exists())
        self.assertIn("workflow_dispatch:", SMOKE)
        self.assertNotIn("pull_request:", SMOKE)
        self.assertEqual(re.findall(r"^  ([\w-]+):$", SMOKE.split("jobs:\n", 1)[1], re.M), ["native-fresh"])
        self.assertEqual(re.findall(r"platform: ([^\n]+)", SMOKE), ["linux/amd64"])
        self.assertIn("runner: ubuntu-24.04", SMOKE)
        self.assertIn("timeout-minutes: 15", SMOKE)
        self.assertIn("go-version: '1.27.4'", SMOKE)
        self.assertLess(SMOKE.index("Record original job clock"), SMOKE.index("uses: actions/checkout"))
        self.assertLess(SMOKE.index("reviewed fresh workload"), SMOKE.index("Require the actual native manager"))
        self.assertLess(SMOKE.index("Require the actual native manager"), SMOKE.index("uses: actions/setup-go"))
        self.assertEqual(SMOKE.count('open(p,"x")'), 1)
        self.assertNotIn("diagnostic", SMOKE)
        self.assertNotIn("MESH_TRANSPORT_LIFECYCLE", SMOKE)
        self.assertNotIn("continue-on-error", SMOKE)
        self.assertIn('job-finish "$RUNNER_TEMP/installed-job-clock.json" "$RUNNER_TEMP/fresh-proof/fixture-clock.json"', SMOKE)
        name = "Prepare the real helper before the first managed daemon activation"
        frozen = re.sub(r"^if \[\[ \$CLOCK_CALIBRATION.*?^fi\n", "", self.script(job("fresh-managed"), name), flags=re.M | re.S)
        smoke = self.script(SMOKE, name)
        self.assertEqual(smoke.strip(), frozen.strip())
        self.assertIn('--native-preflight --fresh-managed', smoke)
        self.assertIn('--clock-mode frozen --clock-job-start "$RUNNER_TEMP/installed-job-clock.json"', frozen)
        script = smoke.replace("python3 scripts/prove-installed-catchup.py", "printf '%s\\n'")
        result = subprocess.run(["bash", "-e", "-c", script], env=os.environ | {"RUNNER_TEMP": "/unused"}, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.check_support_args(result.stdout.splitlines(), "false")

    def test_smoke_exact_pre_admission_rejects_a_workload_mismatch(self):
        script = self.script(SMOKE, "Require the exact source and reviewed fresh workload before native execution")
        source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        with tempfile.TemporaryDirectory(prefix="smoke-admission-") as temporary:
            self.check_smoke_admission(script, source, Path(temporary))

    def check_smoke_admission(self, script, source, root):
        origin = root / "installed-job-clock.json"
        origin.write_text(json.dumps({"origin_ns": clocks.time.monotonic_ns(), "run": "smoke-run", "job": "native-fresh",
                                      "admission_ns": 900 * clocks.SECOND}))
        environment = os.environ | {"MESH_HARNESS_SOURCE": source, "RUNNER_TEMP": str(root), "GITHUB_RUN_ID": "smoke-run",
            "GITHUB_JOB": "native-fresh", "PYTHONPATH": str(ROOT / "scripts"), "PYTHONDONTWRITEBYTECODE": "1",
            "MESH_CATALOG_EVIDENCE": "0"}
        accepted = subprocess.run(["bash", "-e", "-c", script], cwd=ROOT, env=environment, capture_output=True, timeout=5)
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        mismatch = subprocess.run(["bash", "-e", "-c", script.replace("source_host=None", 'source_host="pc"')], cwd=ROOT,
            env=environment, capture_output=True, timeout=5)
        self.assertNotEqual(mismatch.returncode, 0)
        self.assertIn(b"workload differs from actual invocation", mismatch.stderr)
        self.assertEqual(list(root.iterdir()), [origin])

    def test_support_diagnostic_selection_is_explicit_and_cannot_enable_ordinary_fallback(self):
        script = self.script(job("frozen-budgets"), "Select reviewed per-platform and profile job admissions")
        with tempfile.TemporaryDirectory(prefix="support-calibration-") as temporary:
            environment = os.environ | {"CALIBRATION_ONLY": "true", "CALIBRATION_SCOPE": "cohort", "PUBLICATION_ONLY": "false", "HAS_COHORT": "true",
                "CATALOG_EVIDENCE": "false", "PAIRED_SCENARIO": "", "GITHUB_OUTPUT": str(Path(temporary) / "outputs"),
                "PYTHONDONTWRITEBYTECODE": "1"}
            for selection in ("adoption-all", "adoption-interrupt", "fresh"):
                self.run_support_selector(script, environment | {"CALIBRATION_SUPPORT": selection}, 0)
            self.run_support_selector(script, environment | {"CALIBRATION_SUPPORT": "unknown"}, 2)
            self.run_support_selector(script, environment | {"CALIBRATION_SUPPORT": "fresh", "CALIBRATION_ONLY": "false"}, 1)

    def run_support_selector(self, script, environment, expected):
        result = subprocess.run(["bash", "-c", script], cwd=ROOT, env=environment, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, expected, result.stderr)

    def test_support_clock_arguments_use_original_origin_in_both_modes(self):
        cases = (("native-adoption", "Adopt actual published native pairs and verify failed activation restoration"),
                 ("fresh-managed", "Prepare the real helper before the first managed daemon activation"))
        for name, step_name in cases:
            script = self.script(job(name), step_name).replace("python3 scripts/prove-installed-catchup.py", "printf '%s\\n'")
            for calibration in ("true", "false"):
                environment = os.environ | {"RUNNER_TEMP": "/unused", "CLOCK_CALIBRATION": calibration, "PAIRED_SCENARIO": ""}
                result = subprocess.run(["bash", "-c", script], env=environment, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                args = result.stdout.splitlines()
                self.check_support_args(args, calibration)

    def check_support_args(self, args, calibration):
        self.assertIn("/unused/installed-job-clock.json", args)
        self.assertEqual(args[args.index("--clock-mode") + 1], "diagnostic" if calibration == "true" else "frozen")
        self.assertEqual("--clock-command-seconds" in args, calibration == "true")
        if calibration == "true":
            self.assertEqual(args[args.index("--clock-command-seconds") + 1], "1500")
    def test_every_frozen_caller_records_origin_first_and_passes_it(self):
        for name in ("signed-cohort-chain", "native-adoption", "fresh-managed"):
            source = job(name)
            self.assertIn("needs: frozen-budgets", source)
            self.assertIn("outputs.minutes", source)
            self.assertLess(source.index("Record original job clock"), source.index("uses: actions/checkout"))
            self.assertEqual(source.count('open(p,"x")'), 1)
            self.assertNotIn('open(p,"w")', source)
            self.assertIn('--clock-mode frozen --clock-job-start "$RUNNER_TEMP/installed-job-clock.json"', source)
            self.assertIn('int(os.environ["FIXTURE_JOB_MINUTES"])*60*1000000000', source)

    def test_actual_origin_recorder_refuses_reset(self):
        source = job("native-adoption")
        script = next(line.strip() for line in source.splitlines() if line.strip().startswith("python3 -c"))
        with tempfile.TemporaryDirectory(prefix="frozen-origin-") as temporary:
            environment = os.environ | {"RUNNER_TEMP": temporary, "FIXTURE_JOB_MINUTES": "20",
                                        "GITHUB_RUN_ID": "test-run", "GITHUB_JOB": "test-job"}
            first = subprocess.run(["bash", "-c", script], env=environment, capture_output=True, timeout=5)
            self.assertEqual(first.returncode, 0, first.stderr)
            path = Path(temporary) / "installed-job-clock.json"
            original = path.read_bytes()
            self.assertEqual(json.loads(original)["admission_ns"], 1200 * clocks.SECOND)
            second = subprocess.run(["bash", "-c", script], env=environment, capture_output=True, timeout=5)
            self.assertNotEqual(second.returncode, 0)
            self.assertEqual(path.read_bytes(), original)

    def test_calibration_remains_explicit_and_publication_is_preserved(self):
        cohort = job("signed-cohort-chain")
        self.assertIn("inputs.calibration_only && 30", cohort)
        self.assertIn("if [[ $CLOCK_CALIBRATION == true ]]", cohort)
        self.assertIn("--clock-job-seconds 1800 --clock-command-seconds 1500 --clock-external-seconds 240 --clock-emergency-seconds 60", cohort)
        self.assertIn("mode: [oldest, order]", cohort)
        self.assertEqual(cohort.count("            mode: order"), 2)
        self.assertNotIn("continue-on-error", WORKFLOW)
        self.assertIn("--publication-version", job("publication-native"))

    def test_support_external_records_use_the_actual_fixture_output(self):
        self.assertIn('job-finish "$RUNNER_TEMP/installed-job-clock.json" "$RUNNER_TEMP/installed-catchup/pairs/fixture-clock.json"', job("native-adoption"))
        self.assertIn('job-finish "$RUNNER_TEMP/installed-job-clock.json" "$RUNNER_TEMP/fresh-proof/fixture-clock.json"', job("fresh-managed"))
        self.assertIn("CATALOG_EVIDENCE: ${{ inputs.catalog_evidence || !inputs.calibration_only }}", job("frozen-budgets"))
        cohort = job("signed-cohort-chain")
        self.assertEqual(cohort.count("(inputs.catalog_evidence || !inputs.calibration_only) && startsWith(matrix.platform, 'linux/') && matrix.mode == 'oldest'"), 3)

    def test_cli_missing_plan_stops_before_root_or_native_control(self):
        spec = importlib.util.spec_from_file_location("budget_proof", ROOT / "scripts/prove-installed-catchup.py")
        proof = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(proof)
        with tempfile.TemporaryDirectory(prefix="frozen-cli-") as temporary:
            argv = ["proof", temporary, "--native-preflight", "--fresh-managed", "--clock-mode", "frozen",
                    "--clock-plan-index", str(Path(temporary) / "absent.json")]
            with patch.object(sys, "argv", argv), patch.dict(os.environ, MESH_HARNESS_SOURCE="", INSTALLED_CATCHUP_PLATFORM="linux/amd64"), \
                    patch.object(proof.platform, "system", return_value="Linux"), patch.object(proof.platform, "machine", return_value="x86_64"), \
                    patch.object(proof.signal, "signal"):
                self.assert_no_native_dispatch(proof)

    def assert_no_native_dispatch(self, proof):
        with patch.object(proof, "run_owned") as run, self.assertRaisesRegex(ValueError, "frozen budgets inactive"):
            proof.main()
        run.assert_not_called()

    def test_prepared_cli_forwards_only_to_the_selected_worker_role(self):
        spec = importlib.util.spec_from_file_location("prepared_budget_proof", ROOT / "scripts/prove-installed-catchup.py")
        proof = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(proof)
        worker = SimpleNamespace(run_prepare=unittest.mock.Mock(), run_slice=unittest.mock.Mock())
        args = SimpleNamespace(export_prepared=Path("/export"), prepared_releases=None, source_host="pc",
            worker_registry=Path("/registry"), worker_probe=Path("/probe"), installed_slice=Path("/slice"),
            native_helper=unittest.mock.Mock(), format_waypoint=True)
        import fixtures
        with patch.dict(sys.modules, {"fixtures.installed_prepared": worker}), \
                patch.object(fixtures, "installed_prepared", worker, create=True):
            proof.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64", "test-plan")
            self.assertEqual(worker.run_prepare.call_args.args[3]["name"], "pc")
            self.assertTrue(worker.run_prepare.call_args.kwargs["format_waypoint"])
            self.assertEqual(worker.run_prepare.call_args.kwargs["preparer"], args.native_helper.resolve.return_value)
            worker.run_slice.assert_not_called()
            args.export_prepared, args.prepared_releases = None, Path("/bundle")
            proof.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64", "test-plan")
            worker.run_slice.assert_called_once_with(Path("/root"), Path("/out"), "linux/amd64", args.native_helper.resolve.return_value,
                                                     Path("/bundle"), Path("/slice"), clock_plan="test-plan")

    def test_prepared_cli_rejects_incomplete_and_mixed_roles(self):
        spec = importlib.util.spec_from_file_location("prepared_argument_proof", ROOT / "scripts/prove-installed-catchup.py")
        proof = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(proof)
        args = {"export_prepared": None, "prepared_releases": Path("/bundle"), "installed_slice": Path("/slice"),
            "installed_adoption": True, "signed_chain": True, "format_waypoint": False, "ordered_hosts": False, "fresh_managed": False,
            "scenario": None, "prepare_only": False, "publication_version": None, "publication_source": None,
            "native_helper": Path("/preparer")}
        parser = unittest.mock.Mock()
        parser.error.side_effect = ValueError("invalid prepared CLI")
        proof.validate_prepared_arguments(parser, SimpleNamespace(**args))
        parser.error.assert_not_called()
        for changes in ({"installed_slice": None}, {"fresh_managed": True}, {"publication_source": "a" * 40},
                        {"installed_adoption": False}, {"format_waypoint": True}, {"export_prepared": Path("/export")}):
            with self.assertRaisesRegex(ValueError, "invalid prepared CLI"):
                proof.validate_prepared_arguments(parser, SimpleNamespace(**(args | changes)))


if __name__ == "__main__":
    unittest.main()
