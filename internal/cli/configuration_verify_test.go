package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/update"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

func TestCatalogPolicyOldConsumerCannotReportSaved(t *testing.T) {
	directory := t.TempDir()
	store, err := storage.OpenAdvisory(t.Context(), filepath.Join(directory, "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := &SQLiteCatalogCache{store: store, now: time.Now}
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	rows := []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000"}}
	saved, err := cache.SaveServices(t.Context(), host, "", rows)
	if err != nil || !saved {
		t.Fatalf("initial save: %t %v", saved, err)
	}
	err = domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error {
		return gate.Prepare((domainpolicy.Policy{Primary: "new.example"}).Digest(), []string{host.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = cache.SaveServices(t.Context(), host, "", rows)
	if err != nil || saved {
		t.Fatalf("old policy reported a committed snapshot: %t %v", saved, err)
	}
	cached, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(cached) != 0 {
		t.Fatalf("old policy regained rows: %+v %v", cached, err)
	}
}

func TestDomainGenerationProcessHelper(t *testing.T) {
	mode := os.Getenv("MESH_U4_PROCESS")
	if mode == "" {
		t.Skip("owned subprocess helper")
	}
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	host := HostRecord{ID: "remote", MeshIdentity: "remote-identity"}
	rows := []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", PrivateHost: "api.mesh.test"}}
	if mode == "publish-crash" {
		crashDomainPublication(t)
		return
	}
	if mode == "old-writer" {
		pauseOldDomainWriter(t, cache, host, rows)
		return
	}
	cached, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(cached) != 0 {
		t.Fatalf("pending crash invalidation did not settle: %+v %v", cached, err)
	}
	if saved, err := cache.SaveServices(t.Context(), host, "", rows); err != nil || !saved {
		t.Fatalf("new loaded policy cannot publish: %t %v", saved, err)
	}
	cached, err = cache.LoadServices(t.Context(), host)
	if err != nil || len(cached) != 1 {
		t.Fatalf("fresh replacement unavailable: %+v %v", cached, err)
	}
}

func TestDomainGenerationTwoProcessesAndCrashRecovery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("owned Linux process fixture")
	}
	root := t.TempDir()
	state, config := filepath.Join(root, "state"), filepath.Join(root, "config")
	for _, dir := range []string{state, config} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(config, "domains.json")
	initial, _ := json.Marshal(domainpolicy.Current())
	if err := os.WriteFile(policy, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.json"), []byte(`{"version":1,"hosts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	environment := append(testenv.ForProcess(state), "MESH_STATE_DIR="+state, "MESH_CONFIG_DIR="+config, "MESH_TEST_DOMAIN_POLICY="+policy)
	old := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^TestDomainGenerationProcessHelper$", "-test.timeout=10s")
	old.Env = slices.Concat(environment, []string{"MESH_U4_PROCESS=old-writer"})
	input, err := old.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := old.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var problems bytes.Buffer
	old.Stderr = &problems
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		_ = input.Close()
		if !joined {
			_ = old.Process.Kill()
			_ = old.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		ready <- scanner.Scan() && scanner.Text() == "READY"
		for scanner.Scan() {
		}
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("writer did not pause: %s", problems.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old writer did not reach its pre-commit pause")
	}
	publisher := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^TestDomainGenerationProcessHelper$", "-test.timeout=10s")
	publisher.Env = slices.Concat(environment, []string{"MESH_U4_PROCESS=publish-crash"})
	published, err := publisher.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatalf("wrong publisher crash: %v %s", err, published)
	}
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	err = old.Wait()
	joined = true
	if err != nil {
		t.Fatalf("paused old writer was not fenced: %v %s", err, problems.String())
	}
	fresh := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^TestDomainGenerationProcessHelper$", "-test.timeout=10s")
	fresh.Env = slices.Concat(environment, []string{"MESH_U4_PROCESS=fresh-reader"})
	if output, err := fresh.CombinedOutput(); err != nil {
		t.Fatalf("new process recovery: %v %s", err, output)
	}
}

func TestDomainGenerationSnapshotCloneAndQueuedResult(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	initializeTestServicePolicy(t)
	token, err := loadedServicePolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := StateView{Policy: token, PrivateName: "pc.mesh.mesh.test", NameVerified: true, Services: []protocol.ServiceInfo{{Name: "api"}}, Sections: map[string]ObservedSection{}}
	snapshot := &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "api"}}}
	if err := before.applySnapshot(snapshot, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	clone := before.Clone()
	if clone.Policy != token || before.Policy != token {
		t.Fatal("snapshot or clone lost original policy token")
	}
	state, _ := paths.StateDirPath()
	if err := domainpolicy.WithCatalogGate(t.Context(), state, func(gate *domainpolicy.CatalogGate) error {
		return gate.Prepare((domainpolicy.Policy{Primary: "new.test"}).Digest(), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if servicePolicyCurrent(t.Context(), clone.Policy) {
		t.Fatal("queued old result was reclassified as current")
	}
	next := fencePickerPolicy(t.Context(), PickerServicesUpdate{Policy: clone.Policy, Catalog: PickerServiceCatalog{Rows: []ServiceCatalogRow{{Live: true}}}})
	if !next.Catalog.Stale || len(next.Catalog.Rows) != 0 || next.Policy != token {
		t.Fatal("queued picker result regained current status")
	}
	if servicePolicyCurrent(t.Context(), domainpolicy.CatalogToken{}) {
		t.Fatal("absent token became current")
	}
}

func TestDomainGenerationRetainedDashboardStream(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	host := HostRecord{ID: "host", MachineName: "pc", MeshIdentity: "identity"}
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeStateWatch {
			snapshot := reviewSnapshot(host)
			snapshot.StateSnapshot.Services = []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000"}}
			snapshot.StateSnapshot.Current[protocol.TopicServices] = protocol.Observation{}
			return snapshot
		}
		return nil
	})
	monitor := dashboardMonitor{records: []HostRecord{host}, watcher: NewStateWatcher(dial)}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	changed, cleared := false, false
	err := monitor.Run(ctx, func(view DashboardHostView) {
		if !changed && len(view.Services.Rows) > 0 {
			changed = true
			advanceRetainedDomainGeneration(ctx, t)
			return
		}
		if changed && len(view.Services.Rows) == 0 && view.Services.Failing {
			cleared = true
			checkPreservedDashboardState(t, view)
			cancel()
		}
	})
	if err != nil || !changed || !cleared {
		t.Fatalf("retained live stream was not invalidated: %t %t %v", changed, cleared, err)
	}
}

func pauseOldDomainWriter(t *testing.T, cache *SQLiteCatalogCache, host HostRecord, rows []protocol.ServiceInfo) {
	t.Helper()
	if saved, err := cache.SaveServices(t.Context(), host, "", rows); err != nil || !saved {
		t.Fatalf("seed: %t %v", saved, err)
	}
	if _, err := validateRemoteService(rows[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
		t.Fatal(err)
	}
	var release [1]byte
	if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
		t.Fatal(err)
	}
	if saved, err := cache.SaveServices(t.Context(), host, "", rows); err != nil || saved {
		t.Fatalf("old writer committed: %t %v", saved, err)
	}
}

func TestCatalogPolicyTokenIsAssignedOnlyAfterPinnedOwnerRead(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	if saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000"}}); err != nil || !saved {
		t.Fatal(saved, err)
	}
	raw, err := cache.store.ListCachedServices(t.Context(), storage.HostID(host.ID))
	if err != nil || len(raw) != 1 || raw[0].Policy.Generation != 0 {
		t.Fatal(raw, err)
	}
	rows, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(rows) != 1 || rows[0].Policy.Generation == 0 {
		t.Fatal(rows, err)
	}
	token := rows[0].Policy
	catalog := cachedServiceCatalogRows(host, rows)
	if catalog[0].Policy != token || catalog[0].Live || !catalog[0].Stale {
		t.Fatal("cached projection changed token or became live")
	}
	all, err := cache.LoadAllServices(t.Context(), []HostRecord{host})
	if err != nil || len(all[host.ID]) != 1 || all[host.ID][0].Policy != token {
		t.Fatal(all, err)
	}
	wrong := host
	wrong.MeshIdentity = "another-identity"
	denied, err := cache.LoadAllServices(t.Context(), []HostRecord{wrong})
	if err != nil || len(denied) != 0 {
		t.Fatal("cached snapshot bypassed identity pin", denied, err)
	}
	state, _ := paths.StateDirPath()
	if err := domainpolicy.WithCatalogGate(t.Context(), state, func(gate *domainpolicy.CatalogGate) error {
		return gate.Prepare((domainpolicy.Policy{Primary: "new.test"}).Digest(), []string{host.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if servicePolicyCurrent(t.Context(), catalog[0].Policy) || catalog[0].Policy != token {
		t.Fatal("queued cached rows were reclassified")
	}
}

func TestConfigurationMismatchRequiresAValidService(t *testing.T) {
	valid := protocol.ServiceInfo{Name: "api", Kind: "proxy", Target: "3000", PrivateHost: "api.unloaded.example"}
	_, err := validateRemoteService(valid)
	var mismatch *configurationPolicyMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("loaded policy rejection was not typed: %v", err)
	}
	for _, invalid := range []protocol.ServiceInfo{
		{Name: "api", Kind: "unknown", Target: "3000", PrivateHost: valid.PrivateHost},
		{Name: "api", Kind: "proxy", Target: "0", PrivateHost: valid.PrivateHost},
		{Name: "api", Kind: "proxy", Target: "3000", PrivateHost: "api..invalid"},
		{Name: "api", Kind: "proxy", Target: "3000", PrivateHost: valid.PrivateHost, Healthy: true, Problem: "contradictory health"},
	} {
		_, err := validateRemoteService(invalid)
		if err == nil || errors.As(err, &mismatch) {
			t.Fatalf("malformed data became configuration drift: %+v %v", invalid, err)
		}
	}
}

func TestCatalogPolicyPendingPublicationSurvivesReader(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	if saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000"}}); err != nil || !saved {
		t.Fatal(saved, err)
	}
	directory, _ := paths.StateDirPath()
	if err := domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error {
		return gate.Prepare((domainpolicy.Policy{Primary: "new.test"}).Digest(), []string{host.ID})
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if err := domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error {
		if !gate.Policy.Pending {
			t.Error("read cleared a generation whose domain rename never happened")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPolicyIncompleteReplacementCannotReviveOnEqualPolicy(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	row := protocol.ServiceInfo{Name: "api", Kind: "proxy", Target: "3000"}
	if saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{row}); err != nil || !saved {
		t.Fatal(saved, err)
	}
	if saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{row, row}); err == nil || saved {
		t.Fatal("invalid complete replacement was accepted", saved, err)
	}
	raw, err := cache.store.ListCachedServices(t.Context(), storage.HostID(host.ID))
	if err != nil || len(raw) != 1 {
		t.Fatal("replacement failed to roll back", raw, err)
	}
	directory, _ := paths.StateDirPath()
	if err := domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error { return gate.Prepare(gate.Policy.Digest, nil) }); err != nil {
		t.Fatal(err)
	}
	rows, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(rows) != 0 {
		t.Fatal("equal policy bytes revived an incomplete snapshot", rows, err)
	}
	if saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{row}); err != nil || !saved {
		t.Fatal(saved, err)
	}
	rows, err = cache.LoadServices(t.Context(), host)
	if err != nil || len(rows) != 1 {
		t.Fatal("fresh complete replacement did not reconcile", rows, err)
	}
}

func crashDomainPublication(t *testing.T) {
	t.Helper()
	err := withDomainConfiguration(t.Context(), func(file update.DomainFile) error {
		desired := update.DomainValue{Present: true, Policy: domainpolicy.Policy{Primary: "new.test", Aliases: []string{"mesh.test", "old.test"}, LegacyCertificateDomain: "mesh.test"}}
		if _, err := file.Prepare(desired.Digest()); err != nil {
			return fmt.Errorf("publish domain crash fixture: %w", err)
		}
		if err := file.Save(desired); err != nil {
			return fmt.Errorf("publish domain crash fixture: %w", err)
		}
		os.Exit(17)
		return nil
	})
	t.Fatalf("crash boundary not reached: %v", err)
}

func advanceRetainedDomainGeneration(ctx context.Context, t *testing.T) {
	t.Helper()
	state, _ := paths.StateDirPath()
	err := domainpolicy.WithCatalogGate(ctx, state, func(gate *domainpolicy.CatalogGate) error {
		return gate.Prepare((domainpolicy.Policy{Primary: "new.test"}).Digest(), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func checkPreservedDashboardState(t *testing.T, view DashboardHostView) {
	t.Helper()
	if view.Connection != StateReachable || len(view.Sessions.Rows) == 0 {
		t.Error("policy invalidation changed unrelated connection or session state")
	}
}

func initializeTestServicePolicy(t *testing.T) domainpolicy.CatalogToken {
	t.Helper()
	directory, err := paths.StateDirPath()
	if err != nil {
		t.Fatal(err)
	}
	var token domainpolicy.CatalogToken
	err = domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error { token = gate.Policy.CatalogToken; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return token
}
