package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type recoveryFixture struct {
	*fixture
	request HelperRecovery
	prior   helperRecoverySnapshot
	events  string
	tools   string
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := newFixture(t)
	f.engine.cfg.ServiceSpec = ServiceSpec{Kind: "systemd", Name: "mesh.service"}
	f.engine.cfg.Probe = func(ctx context.Context) (Health, error) {
		health, err := f.manager.probe(ctx)
		health.BootID = "fixture-boot"
		return health, err
	}
	cfg := HelperConfig{StateDir: f.engine.cfg.StateDir, Executable: f.engine.cfg.Executable,
		ServiceDir: filepath.Join(t.TempDir(), "services"), Kind: "systemd"}
	installed, err := PrepareHelper(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.stage()
	f.manager.failCandidate = true
	if _, err = f.engine.Grant(t.Context(), f.request.ID, f.request.Generation); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(t.Context())
	if err == nil || status.Phase != RolledBack {
		t.Fatalf("fixture rollback = %s, %v", status.Phase, err)
	}
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err = os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events")
	t.Setenv("MESH_RECOVERY_TEST_EVENTS", events)
	if err = writeRecoveryFixture(filepath.Join(tools, "systemctl"), []byte("#!/bin/sh\nprintf 'helper service mutation\\n' >> \"$MESH_RECOVERY_TEST_EVENTS\"\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg.Executable = filepath.Join(root, "replacement")
	request := HelperRecovery{Helper: cfg, Expected: JournalExpectation{Operation: status.Request.ID,
		Generation: status.Request.Generation, Phase: status.Phase, OriginalDigest: status.Request.Current.Digest},
		Probe: func(ctx context.Context, image HelperInstallation) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, fmt.Errorf("fixture helper probe cancelled: %w", err)
			}
			link, err := os.Readlink(filepath.Join(transactionDir(cfg.StateDir), "helper", "current"))
			if err != nil || link != image.Executable {
				return 0, errors.New("fixture helper image mismatch")
			}
			if err = release.VerifyExecutable(ctx, image.Executable, image.Digest); err != nil {
				return 0, fmt.Errorf("verify fixture helper: %w", err)
			}
			return 123, nil
		},
		CheckIdle: func(context.Context, Status) error { return nil },
	}
	prior, err := recoveryHelperSnapshot(t.Context(), cfg)
	if err != nil || prior.installation != installed {
		t.Fatalf("prior helper snapshot: %v", err)
	}
	r := &recoveryFixture{fixture: f, request: request, prior: prior, events: events, tools: tools}
	r.publishReplacement(t, "v0.3.0", "exit 0")
	return r
}

func (f *recoveryFixture) publishReplacement(t *testing.T, version, journalCheck string) {
	t.Helper()
	binary := []byte(strings.Replace(string(testExecutable(version)), "health=$2",
		"if [ \"$1\" = update-helper ]; then\n"+journalCheck+"\nfi\nhealth=$2", 1))
	if err := writeRecoveryFixture(f.request.Helper.Executable, binary); err != nil {
		t.Fatal(err)
	}
	manifest := f.fixture.request.Manifest
	manifest.Version, manifest.Compatibility.Transitions = version, nil
	archive := archiveBytes(t, binary)
	for index := range manifest.Artifacts {
		manifest.Artifacts[index].BinarySHA256 = digestBytes(binary)
		manifest.Artifacts[index].SHA256 = digestBytes(archive)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(server.Close)
	f.engine.cfg.Client = release.Client{BaseURL: server.URL, HTTPClient: server.Client()}
	f.request.Manifest, f.request.Digest = manifest, digestBytes(binary)
}

func (f *recoveryFixture) assertNoServiceMutation(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.events); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper service mutated before recovery guards passed")
	}
	if !recoveryHelperUnchanged(f.request.Helper.StateDir, f.prior) {
		t.Fatal("prior helper record, service, or launcher changed")
	}
	f.roundTrip()
}

func TestHelperRecoveryPreservesDaemonWorkersAndJournal(t *testing.T) {
	f := newRecoveryFixture(t)
	before, err := f.engine.Read()
	if err != nil {
		t.Fatal(err)
	}
	daemonPID := f.manager.process.Process.Pid
	result, err := f.engine.RecoverHelper(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.PID <= 0 || result.Installation.Digest != f.request.Digest || result.Phase != RolledBack {
		t.Fatal("recovery did not return the verified replacement helper")
	}
	if f.manager.process.Process.Pid != daemonPID {
		t.Fatal("helper-only recovery restarted the daemon")
	}
	if err = f.engine.unchangedRecoveryJournal(before); err != nil {
		t.Fatal(err)
	}
	if err = release.VerifyExecutable(t.Context(), f.engine.cfg.Executable, f.request.Expected.OriginalDigest); err != nil {
		t.Fatal(err)
	}
	if err = release.VerifyExecutable(t.Context(), f.prior.installation.Executable, f.prior.installation.Digest); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(filepath.Join(filepath.Dir(f.prior.installation.Executable), "installed.json"))
	if err != nil || string(retained) != string(f.prior.receipt) {
		t.Fatal("prior helper receipt was not durably retained before promotion")
	}
	f.roundTrip()
	second, err := f.engine.RecoverHelper(t.Context(), f.request)
	if err != nil || second != result {
		t.Fatalf("idempotent verified recovery = %+v, %v", second, err)
	}
}

func TestHelperRecoveryReadinessFailureRestoresPriorHelper(t *testing.T) {
	f := newRecoveryFixture(t)
	originalProbe := f.request.Probe
	f.request.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest == f.request.Digest {
			return 0, errors.New("replacement helper refuses readiness")
		}
		return originalProbe(ctx, installed)
	}
	_, err := f.engine.RecoverHelper(t.Context(), f.request)
	if err == nil || !strings.Contains(err.Error(), "replacement helper refuses readiness") {
		t.Fatalf("replacement readiness failure = %v", err)
	}
	if !recoveryHelperUnchanged(f.request.Helper.StateDir, f.prior) {
		t.Fatal("failed recovery did not restore the exact prior receipt, service, and launcher")
	}
	if pid, err := originalProbe(t.Context(), f.prior.installation); err != nil || pid <= 0 {
		t.Fatal("prior helper was not verified after restoration")
	}
	f.roundTrip()
}

func TestHelperRecoveryCancellationRestoresPriorHelper(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	originalProbe := f.request.Probe
	restored := false
	f.request.Probe = func(probeCtx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest == f.request.Digest {
			cancel()
			return 0, probeCtx.Err()
		}
		if ctx.Err() != nil && probeCtx.Err() == nil {
			restored = true
		}
		return originalProbe(probeCtx, installed)
	}
	_, err := f.engine.RecoverHelper(ctx, f.request)
	if !errors.Is(err, context.Canceled) || !restored || !recoveryHelperUnchanged(f.request.Helper.StateDir, f.prior) {
		t.Fatalf("cancelled recovery restoration = %v, verified=%v", err, restored)
	}
	f.roundTrip()
}

func TestHelperRecoveryRejectsUnsafeStateBeforeMutation(t *testing.T) {
	for _, kind := range []string{"active", "pending", "approval", "digest", "boot", "worker", "helper-pid", "downgrade", "journal-probe"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryFixture(t)
			switch kind {
			case "active":
				f.request.Expected.Phase = Granted
			case "pending":
				if err := os.WriteFile(filepath.Join(f.request.Helper.StateDir, "activation.pending"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "approval":
				f.request.CheckIdle = func(context.Context, Status) error { return errors.New("pending update grant") }
			case "digest":
				f.request.Digest = strings.Repeat("f", 64)
			case "helper-pid":
				f.request.Probe = func(context.Context, HelperInstallation) (int, error) { return 0, nil }
			case "boot", "worker":
				original := f.engine.cfg.Probe
				f.engine.cfg.Probe = func(ctx context.Context) (Health, error) {
					health, err := original(ctx)
					if kind == "boot" {
						health.BootID = "another-boot"
					} else {
						health.Workers[0].PID++
					}
					return health, err
				}
			case "downgrade":
				f.publishReplacement(t, "v0.0.9", "exit 0")
			case "journal-probe":
				f.publishReplacement(t, "v0.3.0", "exit 17")
			}
			if _, err := f.engine.RecoverHelper(t.Context(), f.request); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			f.assertNoServiceMutation(t)
			if kind == "pending" {
				if _, err := os.Stat(filepath.Join(f.request.Helper.StateDir, "activation.pending")); err != nil {
					t.Fatal("recovery deleted the pending activation marker")
				}
			}
		})
	}
}

func TestHelperRecoveryRejectsJournalChangedWhileWaitingForLock(t *testing.T) {
	f := newRecoveryFixture(t)
	lock, err := lockInstallation(t.Context(), f.request.Helper.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.engine.RecoverHelper(t.Context(), f.request); done <- err }()
	time.Sleep(100 * time.Millisecond)
	status, err := f.engine.Read()
	if err != nil {
		unlock(lock)
		t.Fatal(err)
	}
	status.Error = "journal changed while recovery waited"
	if err = f.engine.save(&status); err != nil {
		unlock(lock)
		t.Fatal(err)
	}
	unlock(lock)
	if err = <-done; err == nil || !strings.Contains(err.Error(), "journal changed") {
		t.Fatalf("changed journal accepted: %v", err)
	}
	f.assertNoServiceMutation(t)
}

func TestHelperRecoveryCancelledLockDoesNotMutate(t *testing.T) {
	f := newRecoveryFixture(t)
	lock, err := lockInstallation(t.Context(), f.request.Helper.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err = f.engine.RecoverHelper(ctx, f.request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded recovery lock = %v", err)
	}
	f.assertNoServiceMutation(t)
}

func TestHelperRecoveryJournalProbeCancellationHasNoServiceMutation(t *testing.T) {
	f := newRecoveryFixture(t)
	f.publishReplacement(t, "v0.3.0", "sleep 30 &\nwait")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := f.engine.RecoverHelper(ctx, f.request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("journal probe cancellation = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled journal probe retained its child process or inherited pipe")
	}
	f.assertNoServiceMutation(t)
}

type failRestorationStart struct {
	manager  *processService
	original string
	failed   bool
}

func (s *failRestorationStart) Stop(ctx context.Context) error { return s.manager.Stop(ctx) }
func (s *failRestorationStart) Start(ctx context.Context) error {
	digest, err := fileDigest(s.manager.executable)
	if err != nil {
		return err
	}
	if !s.failed && digest == s.original && s.manager.stops >= 2 {
		s.failed = true
		return errors.New("fixture rejected original service restart")
	}
	return s.manager.Start(ctx)
}

func TestHelperRecoverySettlesOnlyGenuineRollbackRestoration(t *testing.T) {
	f := newRecoveryFixture(t)
	replacementClient := f.engine.cfg.Client
	f.engine.cfg.Client = release.Client{BaseURL: f.server.URL, HTTPClient: f.server.Client()}
	if _, err := f.engine.Retry(t.Context(), f.request.Expected.Operation, f.request.Expected.Generation); err != nil {
		t.Fatal(err)
	}
	f.engine.cfg.Service = &failRestorationStart{manager: f.manager, original: f.request.Expected.OriginalDigest}
	if _, err := f.engine.Grant(t.Context(), f.request.Expected.Operation, f.request.Expected.Generation); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(t.Context())
	if err == nil || status.Phase != RollbackFailed {
		t.Fatalf("genuine restoration failure = %s, %v", status.Phase, err)
	}
	if err = f.manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.awaitOriginal()
	f.request.Expected.Phase = RollbackFailed
	f.engine.cfg.Client = replacementClient
	stops := f.manager.stops
	result, err := f.engine.RecoverHelper(t.Context(), f.request)
	if err != nil || result.Phase != RolledBack || f.manager.stops != stops+1 {
		t.Fatalf("existing engine restoration = %s, %v", result.Phase, err)
	}
	status, err = f.engine.Read()
	if err != nil || status.Verified == nil || status.Phase != RolledBack || status.Error == "" {
		t.Fatal("recovery fabricated or discarded the real rollback receipt")
	}
	f.roundTrip()
}

func TestHelperRecoveryRestartFailureRestoresAndRetainsBothCauses(t *testing.T) {
	for _, restorationFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("restoration-fails-%t", restorationFails), func(t *testing.T) {
			f := newRecoveryFixture(t)
			tools := f.tools
			body := "#!/bin/sh\nif [ \"$2\" != restart ]; then exit 0; fi\n"
			if !restorationFails {
				body += "if [ -e \"$MESH_RECOVERY_TEST_EVENTS\" ]; then exit 0; fi\n"
			}
			body += "printf 'restart rejected\\n' >> \"$MESH_RECOVERY_TEST_EVENTS\"\nexit 17\n"
			if err := writeRecoveryFixture(filepath.Join(tools, "systemctl"), []byte(body)); err != nil {
				t.Fatal(err)
			}
			_, err := f.engine.RecoverHelper(t.Context(), f.request)
			if err == nil || !strings.Contains(err.Error(), "replacement helper did not become ready") {
				t.Fatalf("replacement restart failure = %v", err)
			}
			if strings.Contains(err.Error(), "restart prior helper") != restorationFails {
				t.Fatalf("prior restart failure retained=%v: %v", restorationFails, err)
			}
			if !recoveryHelperUnchanged(f.request.Helper.StateDir, f.prior) {
				t.Fatal("restart failure did not retain the prior service, receipt, and launcher")
			}
			f.roundTrip()
		})
	}
}

func TestHelperRecoveryPriorReadinessFailureRetainsBothCauses(t *testing.T) {
	f := newRecoveryFixture(t)
	originalProbe := f.request.Probe
	candidateTried := false
	f.request.Probe = func(ctx context.Context, installed HelperInstallation) (int, error) {
		if installed.Digest == f.request.Digest {
			candidateTried = true
			return 0, errors.New("replacement helper is not ready")
		}
		if candidateTried {
			return 0, errors.New("prior helper is not ready")
		}
		return originalProbe(ctx, installed)
	}
	_, err := f.engine.RecoverHelper(t.Context(), f.request)
	if err == nil || !strings.Contains(err.Error(), "replacement helper is not ready") || !strings.Contains(err.Error(), "prior helper is not ready") {
		t.Fatalf("readiness failures were discarded: %v", err)
	}
	if !recoveryHelperUnchanged(f.request.Helper.StateDir, f.prior) {
		t.Fatal("prior readiness failure discarded the restored receipt, service, or launcher")
	}
	f.roundTrip()
}

func writeRecoveryFixture(path string, data []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open recovery fixture directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(path)
	if err = root.WriteFile(name, data, 0600); err != nil {
		return fmt.Errorf("write recovery fixture: %w", err)
	}
	if err = root.Chmod(name, 0700); err != nil {
		return fmt.Errorf("make recovery fixture executable: %w", err)
	}
	return nil
}
