package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func generationRegressionPublishPolicy(t *testing.T, policy domainpolicy.Policy) {
	t.Helper()
	err := withDomainConfiguration(t.Context(), func(file update.DomainFile) error {
		if _, err := file.Prepare(policy.Digest()); err != nil {
			return fmt.Errorf("prepare regression policy: %w", err)
		}
		if err := file.Save(update.DomainValue{Present: true, Policy: policy}); err != nil {
			return fmt.Errorf("save regression policy: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceGenerationProcessReopenAfterPolicyCycle(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	before := domainpolicy.Current()
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"hosts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	generationRegressionPublishPolicy(t, before)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	rows := []protocol.ServiceInfo{{Name: "old", Kind: "proxy", Target: "3000"}}
	if saved, err := cache.SaveServices(t.Context(), host, "", rows); err != nil || !saved {
		t.Fatal(saved, err)
	}
	original, err := cache.LoadServices(t.Context(), host)
	if err != nil || len(original) != 1 {
		t.Fatal(original, err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	oldToken := original[0].Policy
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Primary = before.Aliases[0]
	after.Aliases = slices.Concat([]string{before.Primary}, before.Aliases[1:])
	generationRegressionPublishPolicy(t, after)
	generationRegressionPublishPolicy(t, before)
	if servicePolicyCurrent(t.Context(), oldToken) {
		t.Fatal("original token unexpectedly stayed current")
	}
	reopened, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	saved, err := reopened.SaveServices(t.Context(), host, "", rows)
	current, loadErr := reopened.LoadServices(t.Context(), host)
	t.Logf("old=%+v reopened=%+v saved=%t rows=%d error=%v load=%v", oldToken, reopened.policyToken, saved, len(current), err, loadErr)
	if saved || len(current) != 0 || reopened.policyToken != oldToken {
		t.Error("unchanged old process re-entered newer generation through a reopened cache")
	}
	loaded, err := loadedServicePolicy(t.Context())
	if loaded != oldToken || !errors.Is(err, errCatalogPolicyChanged) {
		t.Fatalf("new consumer lost the process pin: %+v %v", loaded, err)
	}
	generationRegressionWatcherToken(t, host, oldToken)
}

func TestServiceGenerationBusyGateRetainsLiveData(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	token := initializeTestServicePolicy(t)
	row := ServiceCatalogRow{Policy: token, Service: protocol.ServiceInfo{Name: "live", Kind: "proxy", Target: "3000"}, Live: true}
	before := PickerServicesUpdate{Policy: token, Catalog: PickerServiceCatalog{Rows: []ServiceCatalogRow{row}}}
	if got := fencePickerPolicy(t.Context(), before); len(got.Catalog.Rows) != 1 {
		t.Fatal("positive control had no live row")
	}
	state, _ := paths.StateDirPath()
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- domainpolicy.WithCatalogGate(t.Context(), state, func(gate *domainpolicy.CatalogGate) error {
			close(ready)
			<-release
			return nil
		})
	}()
	<-ready
	held := true
	defer func() {
		if held {
			close(release)
			<-done
		}
	}()
	readCtx, cancel := context.WithTimeout(t.Context(), catalogCacheReadTimeout)
	defer cancel()
	got := fencePickerPolicy(readCtx, before)
	if readCtx.Err() != nil {
		t.Error("live publication waited on advisory writer admission")
	}
	close(release)
	held = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !servicePolicyCurrent(t.Context(), token) {
		t.Fatal("generation changed despite no policy write")
	}
	t.Logf("unchanged token current=true, contention result rows=%d stale=%t problem=%q", len(got.Catalog.Rows), got.Catalog.Stale, got.Problem)
	if len(got.Catalog.Rows) != 1 || got.Catalog.Stale || !got.Catalog.Rows[0].Live || got.Policy != token || got.Problem != "" {
		t.Error("temporary advisory gate contention erased or downgraded a valid live observation")
	}
}

func generationRegressionWatcherToken(t *testing.T, host HostRecord, token domainpolicy.CatalogToken) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := NewStateWatcher(func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("isolated unavailable host")
	})
	var observed StateView
	_ = watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicServices}}, func(view StateView) {
		observed = view
		cancel()
	})
	if observed.Policy != token {
		t.Fatalf("new watcher rebound process generation: %+v", observed.Policy)
	}
}

func TestServiceGenerationLogicalInitialPinSurvivesLazyCache(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	before := domainpolicy.Current()
	token, err := loadedServicePolicy(t.Context())
	if err != nil || token.Generation != domainpolicy.InitialCatalogGeneration {
		t.Fatal(token, err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive read created state: %v", err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"hosts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Primary = before.Aliases[0]
	after.Aliases = slices.Concat([]string{before.Primary}, before.Aliases[1:])
	generationRegressionPublishPolicy(t, after)
	generationRegressionPublishPolicy(t, before)
	store, err := storage.OpenAdvisory(t.Context(), filepath.Join(state, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	cache := &SQLiteCatalogCache{store: store, now: time.Now}
	t.Cleanup(func() { _ = cache.Close() })
	host := HostRecord{ID: "host", MeshIdentity: "identity"}
	saved, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{{Name: "old", Kind: "proxy", Target: "3000"}})
	if err != nil || saved || cache.policyToken != token {
		t.Fatalf("lazy cache admitted a newer generation: saved=%t token=%+v err=%v", saved, cache.policyToken, err)
	}
	generationRegressionWatcherToken(t, host, token)
}

func TestServiceGenerationUnavailablePreservesProvenance(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("MESH_STATE_DIR", directory)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	token := initializeTestServicePolicy(t)
	now := time.Now()
	state := StateView{Policy: token, PrivateName: "pc.mesh.mesh.test", NameVerified: true, Connection: StateReachable,
		Services: []protocol.ServiceInfo{{Name: "live", Kind: "proxy", Target: "3000"}}, Sessions: []protocol.SessionInfo{{ID: "session", State: "running"}}, LastReply: now,
		Sections: map[string]ObservedSection{protocol.TopicHost: {ReceivedAt: now}, protocol.TopicServices: {ReceivedAt: now}}}
	picker := PickerServicesUpdate{Policy: token, Catalog: PickerServiceCatalog{Rows: liveServiceCatalogRows(HostRecord{}, remoteServiceSnapshot{Policy: token, Services: state.Services})}}
	dashboard := dashboardMonitorUpdate{policy: token, view: projectDashboardState(DashboardHost{}, state)}
	if !servicePolicyCurrent(t.Context(), token) {
		t.Fatal("positive control was not current")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	contents, err := root.ReadFile("catalog-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("catalog-policy.json", []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if servicePolicyCurrent(t.Context(), token) {
		t.Fatal("inaccessible generation was claimed current")
	}
	loaded, err := loadedServicePolicy(t.Context())
	if err == nil || loaded != token || errors.Is(err, errCatalogPolicyChanged) {
		t.Fatalf("unknown observation lost its original pin or claimed change: %+v %v", loaded, err)
	}
	unknownState := fenceServiceState(t.Context(), state)
	unknownPicker := fencePickerPolicy(t.Context(), picker)
	unknownDashboard := fenceDashboardPolicy(t.Context(), dashboard)
	if unknownState.Policy != token || len(unknownState.Services) != 1 || !unknownState.Sections[protocol.TopicServices].Observation.Failing || len(unknownState.Sessions) != 1 {
		t.Fatalf("unknown state destroyed provenance: %+v", unknownState)
	}
	if unknownPicker.Policy != token || len(unknownPicker.Catalog.Rows) != 1 || unknownPicker.Catalog.Rows[0].Live || !unknownPicker.Catalog.Stale {
		t.Fatalf("unknown picker destroyed provenance or claimed live: %+v", unknownPicker)
	}
	if unknownDashboard.policy != token || len(unknownDashboard.view.Services.Rows) != 1 || !unknownDashboard.view.Services.Failing {
		t.Fatalf("unknown dashboard destroyed provenance: %+v", unknownDashboard)
	}
	if !picker.Catalog.Rows[0].Live || state.Sections[protocol.TopicServices].Observation.Failing {
		t.Fatal("fencing mutated the queued original observation")
	}
	if err := root.WriteFile("catalog-policy.json", contents, 0600); err != nil {
		t.Fatal(err)
	}
	if !servicePolicyCurrent(t.Context(), token) {
		t.Fatal("same generation did not recover after observation failure")
	}
}

func TestServiceGenerationPendingAndZeroStayInvalid(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	token := initializeTestServicePolicy(t)
	if !servicePolicyCurrent(t.Context(), token) {
		t.Fatal("positive control was not current")
	}
	directory, _ := paths.StateDirPath()
	err := domainpolicy.WithCatalogGate(t.Context(), directory, func(gate *domainpolicy.CatalogGate) error {
		gate.Policy.Pending = true
		return gate.Save()
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := observeServicePolicy(t.Context(), domainpolicy.CatalogToken{})
	if status != servicePolicyUnavailable || err == nil || errors.Is(err, errCatalogPolicyChanged) {
		t.Fatal("missing provenance was classified as a proven policy change", status, err)
	}
	for _, candidate := range []domainpolicy.CatalogToken{token, {}} {
		if servicePolicyCurrent(t.Context(), candidate) {
			t.Fatal("pending or zero generation became current")
		}
		update := fencePickerPolicy(t.Context(), PickerServicesUpdate{Policy: candidate, Catalog: PickerServiceCatalog{Rows: []ServiceCatalogRow{{Policy: candidate, Live: true}}}})
		if len(update.Catalog.Rows) != 0 || !update.Catalog.Stale || update.Policy != candidate {
			t.Fatal("pending or untagged rows survived as current")
		}
	}
}
