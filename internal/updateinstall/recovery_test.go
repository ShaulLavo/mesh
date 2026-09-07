package updateinstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/updategate"
)

type crashService struct {
	point         string
	stops, starts int
}

func (s *crashService) Stop(context.Context) error {
	s.stops++
	if s.point == "stop" || (s.point == "rollback-stop" && s.stops == 2) {
		os.Exit(86)
	}
	return nil
}
func (s *crashService) Start(context.Context) error {
	s.starts++
	if s.point == "start" || (s.point == "rollback-start" && s.starts == 2) {
		os.Exit(86)
	}
	if s.point == "rollback-stop" || s.point == "rollback-start" {
		return errors.New("injected candidate service failure")
	}
	return nil
}

func TestInstallerCrashProcess(t *testing.T) {
	stateDir := os.Getenv("MESH_INSTALL_CRASH_STATE")
	if stateDir == "" {
		return
	}
	status, err := Read(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := status.Settings.Config()
	cfg.Service = &crashService{point: os.Getenv("MESH_INSTALL_CRASH_POINT")}
	cfg.Probe = func(context.Context) (Health, error) { return status.Original, nil }
	engine, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = engine.Run(context.Background())
	t.Fatal("fault injection did not terminate installer")
}

func TestProcessDeathResumesBeforeAndAfterExecutableRename(t *testing.T) {
	for _, point := range []string{"stop", "start", "rollback-stop", "rollback-start"} {
		t.Run(point, func(t *testing.T) { testProcessDeath(t, point) })
	}
}

func testProcessDeath(t *testing.T, point string) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	if point != "stop" {
		if err := updategate.Set(f.engine.cfg.StateDir, f.request.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestInstallerCrashProcess$") //nolint:gosec // launches this test binary for process-death fault injection
	command.Env = append(os.Environ(), "MESH_INSTALL_CRASH_STATE="+f.engine.cfg.StateDir, "MESH_INSTALL_CRASH_POINT="+point)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("crash subprocess: %v %s", err, output)
	}
	if err = updategate.Check(f.engine.cfg.StateDir); !errors.Is(err, updategate.ErrUpdating) {
		t.Fatalf("process death lost gate: %v", err)
	}
	status, err := f.engine.Run(context.Background())
	want := Committed
	if point == "rollback-stop" || point == "rollback-start" {
		want = RolledBack
	}
	if status.Phase != want || (want == Committed && err != nil) {
		t.Fatalf("resume = %s %v", status.Phase, err)
	}
	f.roundTrip()
}

func TestDurablePhasesReopenWithoutReusingActivationAuthority(t *testing.T) {
	for _, phase := range []Phase{Accepted, Staged, Granted, Committed, RolledBack, Cancelled} {
		t.Run(string(phase), func(t *testing.T) { testDurablePhase(t, phase) })
	}
}

func testDurablePhase(t *testing.T, phase Phase) {
	f := newFixture(t)
	ctx := context.Background()
	status, err := f.engine.accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if phase != Accepted {
		status = f.stage()
	}
	if phase == Cancelled {
		status, err = f.engine.Cancel(ctx, f.request.ID, 1)
	}
	if phase == Granted || phase == Committed || phase == RolledBack {
		status, err = f.engine.Grant(ctx, f.request.ID, 1)
	}
	if err != nil {
		t.Fatal(err)
	}
	if phase == Committed || phase == RolledBack {
		f.manager.failCandidate = phase == RolledBack
		status, err = f.engine.Run(ctx)
		if phase == Committed && err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != phase {
		t.Fatalf("phase setup = %s, wanted %s", status.Phase, phase)
	}
	reopened, err := New(f.engine.cfg)
	if err != nil {
		t.Fatal(err)
	}
	stops := f.manager.stops
	status, err = reopened.Run(ctx)
	if phase == Accepted || phase == Staged {
		if status.Phase != Staged || !errors.Is(err, ErrNoGrant) {
			t.Fatalf("staging recovered authority without grant: %s %v", status.Phase, err)
		}
	} else if phase == Granted {
		if status.Phase != Committed || err != nil {
			t.Fatalf("persisted grant did not resume: %s %v", status.Phase, err)
		}
	} else if status.Phase != phase || err != nil {
		t.Fatalf("terminal receipt changed: %s %v", status.Phase, err)
	}
	if phase != Granted && f.manager.stops != stops {
		t.Fatal("receipt reopening restarted daemon")
	}
	f.roundTrip()
}

func TestCorruptStagedExecutableCannotActivate(t *testing.T) {
	f := newFixture(t)
	status := f.stage()
	if err := os.WriteFile(status.Candidate, []byte("corrupted"), 0755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err == nil || status.Phase != RolledBack {
		t.Fatalf("corruption result = %s %v", status.Phase, err)
	}
	if err = verifyFile(f.engine.cfg.Executable, f.request.Current.Digest); err != nil {
		t.Fatal(err)
	}
	f.roundTrip()
}

func TestStaleGrantedRequestNeverOverwritesExternallyUpdatedExecutable(t *testing.T) {
	f := newFixture(t)
	f.stage()
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	newer := testExecutable("v0.3.0")
	if err := atomicWrite(f.engine.cfg.Executable, newer, 0755); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err == nil || status.Phase != Failed {
		t.Fatalf("stale activation = %s %v", status.Phase, err)
	}
	if err = verifyFile(f.engine.cfg.Executable, digestBytes(newer)); err != nil {
		t.Fatal(err)
	}
	if f.manager.stops != 0 {
		t.Fatal("stale activation stopped current daemon")
	}
}

func TestHelperKeepsIndependentBinaryAndPreservesExistingServiceConfiguration(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "mesh")
	if err := os.WriteFile(source, testExecutable("v0.1.0"), 0755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	cfg := HelperConfig{StateDir: filepath.Join(root, "state with spaces"), Executable: source, ServiceDir: filepath.Join(root, "units"), Kind: "systemd"}
	if err := os.MkdirAll(cfg.ServiceDir, 0700); err != nil {
		t.Fatal(err)
	}
	custom := []byte("[Service]\nEnvironment=CUSTOM=yes\nKillMode=process\n")
	if err := os.WriteFile(filepath.Join(cfg.ServiceDir, "mesh.service"), custom, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := PrepareHelper(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, testExecutable("v0.2.0"), 0755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	again, err := PrepareHelper(cfg)
	if err != nil || first.Executable != again.Executable {
		t.Fatalf("uncommitted helper replacement: %+v %v", again, err)
	}
	if err = verifyFile(first.Executable, first.Digest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.ServiceDir, "mesh.service"))
	if err != nil || string(got) != string(custom) {
		t.Fatalf("daemon configuration changed: %q %v", got, err)
	}
	link, err := filepath.EvalSymlinks(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
	if err != nil || link != first.Executable {
		t.Fatalf("helper launcher = %s %v", link, err)
	}
}
