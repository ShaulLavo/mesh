package mcphost

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcphost/mcpfixture"
	"github.com/shaul/mesh/internal/storage"
)

// memStore is an in-memory Store with the same compare-and-swap rules.
type memStore struct {
	mu        sync.Mutex
	services  map[string]storage.MCPServiceRow
	instances map[string]storage.MCPInstanceRow
}

func newMemStore() *memStore {
	return &memStore{services: map[string]storage.MCPServiceRow{}, instances: map[string]storage.MCPInstanceRow{}}
}

func (m *memStore) ListMCPServices(context.Context) ([]storage.MCPServiceRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.MCPServiceRow
	for _, row := range m.services {
		out = append(out, row)
	}
	return out, nil
}

func (m *memStore) PutMCPService(_ context.Context, row storage.MCPServiceRow, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.services[row.ID].Revision != expected {
		return storage.ErrMCPRevision
	}
	m.services[row.ID] = row
	return nil
}

func (m *memStore) DeleteMCPService(_ context.Context, id string, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.services[id].Revision != expected {
		return storage.ErrMCPRevision
	}
	delete(m.services, id)
	return nil
}

func (m *memStore) ListMCPInstances(context.Context) ([]storage.MCPInstanceRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.MCPInstanceRow
	for _, row := range m.instances {
		out = append(out, row)
	}
	return out, nil
}

func (m *memStore) PutMCPInstance(_ context.Context, row storage.MCPInstanceRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[row.ServiceID]; !ok {
		return storage.ErrMCPRevision
	}
	m.instances[row.ID] = row
	return nil
}

func (m *memStore) DeleteMCPInstance(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, id)
	return nil
}

// fakeWorker is an in-process fixture that, like a real worker, keeps
// running and draining its stdout while no controller is attached.
type fakeWorker struct {
	stdin  *io.PipeWriter
	lines  chan []byte
	stderr *syncBuffer
	stop   func()
	ended  chan struct{}
	env    []string
}

type fakeLauncher struct {
	mu        sync.Mutex
	workers   map[string]*fakeWorker
	launches  int
	uncertain bool
	locate    WorkerStatus
	mode      string
}

func newFakeLauncher() *fakeLauncher { return &fakeLauncher{workers: map[string]*fakeWorker{}} }

func (f *fakeLauncher) Launch(_ context.Context, spec LaunchSpec) (WorkerRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches++
	if f.uncertain {
		return WorkerRef{}, ErrLaunchUncertain
	}
	env := map[string]string{mcpfixture.EnvMode: f.mode}
	for _, kv := range spec.Env {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	w := startFakeWorker(func(name string) string { return env[name] })
	w.env = spec.Env
	key := spec.Key.Opaque()
	f.workers[key] = w
	return WorkerRef{SessionID: key, Instance: key, LaunchDigest: key}, nil
}

func startFakeWorker(getenv func(string) string) *fakeWorker {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	w := &fakeWorker{stdin: inW, lines: make(chan []byte, 4096), stderr: &syncBuffer{}, ended: make(chan struct{})}
	go func() {
		_ = mcpfixture.RunEnv(inR, outW, w.stderr, getenv)
		_ = outW.Close()
	}()
	go func() {
		defer close(w.ended)
		reader := bufio.NewReaderSize(outR, 64<<10)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			w.lines <- bytes.TrimSuffix(line, []byte("\n"))
		}
	}()
	var once sync.Once
	w.stop = func() { once.Do(func() { _ = inW.Close(); _ = outR.Close(); <-w.ended }) }
	return w
}

func (f *fakeLauncher) worker(key string) *fakeWorker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workers[key]
}

func (f *fakeLauncher) Locate(_ context.Context, key LaunchKey) (WorkerRef, WorkerStatus, *int, error) {
	f.mu.Lock()
	override := f.locate
	f.mu.Unlock()
	if override != WorkerAbsent {
		return WorkerRef{}, override, nil, nil
	}
	w := f.worker(key.Opaque())
	if w == nil {
		return WorkerRef{}, WorkerAbsent, nil, nil
	}
	ref := WorkerRef{SessionID: key.Opaque(), Instance: key.Opaque(), LaunchDigest: key.Opaque()}
	select {
	case <-w.ended:
		code := 0
		return ref, WorkerEnded, &code, nil
	default:
		return ref, WorkerLive, nil, nil
	}
}

func (f *fakeLauncher) Stop(_ context.Context, ref WorkerRef) error {
	if w := f.worker(ref.SessionID); w != nil {
		w.stop()
	}
	return nil
}

func (f *fakeLauncher) Stderr(_ context.Context, ref WorkerRef, limit int) ([]byte, error) {
	w := f.worker(ref.SessionID)
	if w == nil {
		return nil, nil
	}
	out := []byte(w.stderr.String())
	return out[max(0, len(out)-limit):], nil
}

func (f *fakeLauncher) Dial(_ context.Context, ref WorkerRef) (LineConn, error) {
	w := f.worker(ref.SessionID)
	if w == nil {
		return nil, errors.New("no worker")
	}
	// Attach at the tail, as the real worker attach does for an adopter.
	for len(w.lines) > 0 {
		<-w.lines
	}
	return &fakeAttachment{w: w, done: make(chan struct{})}, nil
}

func (f *fakeLauncher) stopAll() {
	f.mu.Lock()
	workers := slices.Collect(mapsValues(f.workers))
	f.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
}

func mapsValues(m map[string]*fakeWorker) func(func(*fakeWorker) bool) {
	return func(yield func(*fakeWorker) bool) {
		for _, w := range m {
			if !yield(w) {
				return
			}
		}
	}
}

type fakeAttachment struct {
	w    *fakeWorker
	mu   sync.Mutex
	done chan struct{}
	once sync.Once
}

func (a *fakeAttachment) WriteLine(_ context.Context, line []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
		return ErrNotSent
	default:
	}
	if _, err := a.w.stdin.Write(line); err != nil {
		return fmt.Errorf("fake worker: %w", ErrOutcomeUnknown)
	}
	return nil
}

func (a *fakeAttachment) ReadLine() ([]byte, error) {
	select {
	case line := <-a.w.lines:
		return line, nil
	case <-a.done:
		return nil, io.EOF
	case <-a.w.ended:
		return nil, ErrStdoutClosed
	}
}

func (a *fakeAttachment) Close() error {
	a.once.Do(func() { close(a.done) })
	return nil
}

type supervisorFixture struct {
	store    *memStore
	launcher *fakeLauncher
	sup      *Supervisor
	accounts map[string]bool
	jobs     map[string]bool
	mu       sync.Mutex
}

func newSupervisorFixture(t *testing.T) *supervisorFixture {
	t.Helper()
	f := &supervisorFixture{store: newMemStore(), launcher: newFakeLauncher(), accounts: map[string]bool{}, jobs: map[string]bool{}}
	f.sup = f.open(t)
	t.Cleanup(f.launcher.stopAll)
	return f
}

func (f *supervisorFixture) open(t *testing.T) *Supervisor {
	t.Helper()
	sup, err := NewSupervisor(Config{Store: f.store, Launcher: f.launcher, HostDefaultCwd: t.TempDir(),
		AccountCurrent: func(a string) bool { f.mu.Lock(); defer f.mu.Unlock(); return !f.accounts[a] },
		JobActive:      func(_ context.Context, _, job string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.jobs[job] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)
	return sup
}

func (f *supervisorFixture) publish(t *testing.T, d Definition) Definition {
	t.Helper()
	if d.Argv == nil {
		d.Argv = []string{"fixture"}
	}
	out, err := f.sup.Publish(t.Context(), d, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func caller(app string) Caller { return Caller{Account: "device:" + app, App: app, Grant: "g-" + app} }

func whoami(t *testing.T, sup *Supervisor, c Caller, target Target) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	out := &collected{}
	err := sup.Forward(ctx, c, target, callRequest(t, `1`, toolParams("whoami", `{}`, "")), out.emit)
	return out.last(), err
}

func instanceOf(t *testing.T, reply string) string {
	t.Helper()
	_, rest, ok := strings.Cut(reply, "instance=")
	if !ok {
		t.Fatalf("no instance in %s", reply)
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

func TestIsolatedCallersGetSeparateInstances(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	a, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := whoami(t, f.sup, caller("b"), Target{Service: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if instanceOf(t, a) == instanceOf(t, b) {
		t.Fatal("two application callers on one device shared an isolated instance")
	}
	// A fresh connection of the same caller reaches its existing instance.
	again, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"})
	if err != nil || instanceOf(t, again) != instanceOf(t, a) {
		t.Fatalf("reconnection: %s %v", again, err)
	}
	if f.launcher.launches != 2 {
		t.Fatalf("launches = %d", f.launcher.launches)
	}
	// Another caller cannot select a's instance by its handle.
	view, _ := f.sup.Service(caller("a"), "repo")
	if _, err := whoami(t, f.sup, caller("b"), Target{Service: "repo", Instance: view.Instances[0].ID}); !errors.Is(err, ErrDenied) {
		t.Fatalf("handle swap: %v", err)
	}
	if got := len(view.Instances); got != 1 {
		t.Fatalf("caller a sees %d instances", got)
	}
}

func TestSharedServiceUsesOneInstance(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "search", InstancePolicy: PolicyShared})
	a, err := whoami(t, f.sup, caller("a"), Target{Service: "search"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := whoami(t, f.sup, caller("b"), Target{Service: "search"})
	if err != nil || instanceOf(t, a) != instanceOf(t, b) {
		t.Fatalf("shared: %s / %s %v", a, b, err)
	}
	if f.launcher.launches != 1 {
		t.Fatalf("launches = %d", f.launcher.launches)
	}
}

func TestSecretsReachTheProcessButNotMetadata(t *testing.T) {
	f := newSupervisorFixture(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.publish(t, Definition{Name: "noisy", SecretEnv: map[string]string{mcpfixture.EnvSecret: secret}, AccountMode: AccountHostAccount, AccountLabel: "ci bot"})
	reply, err := whoami(t, f.sup, caller("a"), Target{Service: "noisy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "s3cr3t") {
		t.Fatalf("secret in reply: %s", reply)
	}
	for _, c := range []Caller{caller("a"), {Account: "local", Admin: true}} {
		views := fmt.Sprintf("%+v", f.sup.Services(c))
		if strings.Contains(views, "s3cr3t") || strings.Contains(views, secret) {
			t.Fatalf("secret or its path in the catalog: %s", views)
		}
	}
	logs, err := f.sup.Logs(t.Context(), caller("a"), Target{Service: "noisy"}, 0)
	if err != nil || !strings.Contains(string(logs), "s3cr3t-value") {
		t.Fatalf("the owner's diagnostics should show the server's own stderr: %q %v", logs, err)
	}
	if _, err := f.sup.Logs(t.Context(), caller("b"), Target{Service: "noisy", Instance: f.sup.Services(caller("a"))[0].Instances[0].ID}, 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("another caller read diagnostics: %v", err)
	}
}

func TestSecretFileRules(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	loose := filepath.Join(dir, "loose")
	link := filepath.Join(dir, "link")
	for path, mode := range map[string]os.FileMode{good: 0o600, loose: 0o644} {
		if err := os.WriteFile(path, []byte("v"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if value, err := readSecret(good); err != nil || value != "v" {
		t.Fatalf("good secret: %q %v", value, err)
	}
	for _, path := range []string{loose, link, dir, filepath.Join(dir, "missing")} {
		_, err := readSecret(path)
		if !errors.Is(err, ErrSecretFile) || strings.Contains(err.Error(), dir) {
			t.Fatalf("secret %s: %v", path, err)
		}
	}
}

func TestDefinitionPolicy(t *testing.T) {
	cases := map[string]Definition{
		"per-user OAuth":            {Name: "a", Argv: []string{"x"}, AccountMode: AccountPerUser},
		"credential without mode":   {Name: "a", Argv: []string{"x"}, SecretEnv: map[string]string{"T": "/s"}},
		"host account without name": {Name: "a", Argv: []string{"x"}, AccountMode: AccountHostAccount},
		"remote URL":                {Name: "a", URL: "http://10.0.0.1/mcp", InstancePolicy: PolicyShared},
		"managed HTTP isolated":     {Name: "a", URL: "http://127.0.0.1:1/mcp", RunArgv: []string{"x"}},
		"bad name":                  {Name: "A B", Argv: []string{"x"}},
		"relative cwd":              {Name: "a", Argv: []string{"x"}, Cwd: "rel"},
		"reserved header":           {Name: "a", URL: "http://127.0.0.1:1/mcp", InstancePolicy: PolicyShared, AccountMode: AccountHostAccount, AccountLabel: "x", HeaderSecrets: map[string]string{"Mcp-Session-Id": "/s"}},
	}
	for name, d := range cases {
		if _, err := Normalize(d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	d, err := Normalize(Definition{Name: "ok", Argv: []string{"x"}})
	if err != nil || d.InstancePolicy != PolicyIsolated || d.Idle != DefaultIdle || d.AccountMode != AccountNone {
		t.Fatalf("defaults: %+v %v", d, err)
	}
	f := newSupervisorFixture(t)
	if _, err := f.sup.Publish(t.Context(), Definition{Name: "web", URL: "http://127.0.0.1:9/mcp", InstancePolicy: PolicyShared}, 0); !errors.Is(err, ErrHTTPUnavailable) && newHTTPUpstream == nil {
		t.Fatalf("HTTP definition without the H5 client: %v", err)
	}
}

func TestRetiredAccountLosesIsolatedInstancesOnly(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	f.publish(t, Definition{Name: "shared", InstancePolicy: PolicyShared})
	for _, svc := range []string{"repo", "shared"} {
		if _, err := whoami(t, f.sup, caller("a"), Target{Service: svc}); err != nil {
			t.Fatal(err)
		}
	}
	revoked := caller("a")
	revoked.Current = func() bool { return false }
	if _, err := whoami(t, f.sup, revoked, Target{Service: "shared"}); Classify(err).Code != codeAccessDenied {
		t.Fatalf("revoked caller: %v", err)
	}
	f.mu.Lock()
	f.accounts["device:a"] = true
	f.mu.Unlock()
	f.sup.Sweep(t.Context())
	admin := Caller{Account: "local", Admin: true}
	for _, v := range f.sup.Services(admin) {
		want := StateReady
		if v.Name == "repo" {
			want = StateStopped
		}
		if v.Instances[0].State != want {
			t.Fatalf("%s after revocation: %+v", v.Name, v.Instances[0])
		}
	}
}

func TestIdleStopWaitsForJobLease(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", Idle: 50 * time.Millisecond})
	f.jobs["job-1"] = true
	if _, _, err := f.sup.Lease(t.Context(), caller("a"), Target{Service: "repo"}, "", time.Hour, "job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if state := f.sup.Services(caller("a"))[0].Instances[0].State; state != StateReady {
		t.Fatalf("leased instance went %s", state)
	}
	f.mu.Lock()
	f.jobs["job-1"] = false
	f.mu.Unlock()
	f.sup.Sweep(t.Context())
	eventually(t, func() bool { return f.sup.Services(caller("a"))[0].Instances[0].State == StateStopped })
	if _, _, err := f.sup.Lease(t.Context(), caller("a"), Target{Service: "repo"}, "", time.Hour, "job-2"); err == nil {
		t.Fatal("lease named a job that is not running")
	}
}

func TestIdleTimerWaitsForAcknowledgedListen(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", Idle: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan string, 4)
	listen := parseRequest(t, `{"jsonrpc":"2.0","id":"L","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	done := make(chan error, 1)
	go func() {
		done <- f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, listen, func(raw []byte, _ bool) error { events <- string(raw); return nil })
	}()
	waitEvent(t, events)
	time.Sleep(200 * time.Millisecond)
	if state := f.sup.Services(caller("a"))[0].Instances[0].State; state != StateReady {
		t.Fatalf("listening instance went %s", state)
	}
	cancel()
	<-done
	eventually(t, func() bool { return f.sup.Services(caller("a"))[0].Instances[0].State == StateStopped })
}

func TestDaemonRestartAdoptsTheExactWorker(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	before, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	// An in-flight call when the daemon goes away reports an unknown outcome.
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	slow := make(chan error, 1)
	go func() {
		slow <- f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, callRequest(t, `5`, toolParams("slow", `{}`, "")), (&collected{}).emit)
	}()
	time.Sleep(50 * time.Millisecond)
	f.sup.Close()
	if err := <-slow; Classify(err).Code != codeOutcomeUnknown {
		t.Fatalf("in-flight call across restart: %v", err)
	}
	next := f.open(t)
	after, err := whoami(t, next, caller("a"), Target{Service: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if instanceOf(t, after) != instanceOf(t, before) || f.launcher.launches != 1 {
		t.Fatalf("adoption launched again: %s vs %s, launches %d", before, after, f.launcher.launches)
	}
}

func TestUncertainLaunchIsNeverRepeated(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo"})
	f.launcher.uncertain = true
	for range 3 {
		if _, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); Classify(err).Code != codeUnavailable {
			t.Fatalf("uncertain launch: %v", err)
		}
	}
	if f.launcher.launches != 1 {
		t.Fatalf("launches = %d", f.launcher.launches)
	}
	// After a restart, recovery still cannot see the worker and keeps refusing.
	f.sup.Close()
	f.launcher.locate = WorkerUncertain
	next := f.open(t)
	if _, err := whoami(t, next, caller("a"), Target{Service: "repo"}); Classify(err).Code != codeUnavailable {
		t.Fatalf("uncertain after restart: %v", err)
	}
	if f.launcher.launches != 1 {
		t.Fatalf("launches after restart = %d", f.launcher.launches)
	}
}

func TestLegacyServerIsIncompatible(t *testing.T) {
	f := newSupervisorFixture(t)
	f.launcher.mode = "legacy"
	f.publish(t, Definition{Name: "old"})
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "old"}); Classify(err).Code != codeIncompatible {
		t.Fatalf("legacy server: %v", err)
	}
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "old"}); Classify(err).Code != codeIncompatible || f.launcher.launches != 1 {
		t.Fatalf("incompatible server was retried: %v, launches %d", err, f.launcher.launches)
	}
}

func TestBadResultsAfterProbeFailOnlyThatCall(t *testing.T) {
	f := newSupervisorFixture(t)
	f.launcher.mode = "bad-after-probe"
	f.publish(t, Definition{Name: "sloppy"})
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "sloppy"}); Classify(err).Code != codeOutcomeUnknown {
		t.Fatalf("result without resultType: %v", err)
	}
	if state := f.sup.Services(caller("a"))[0].Instances[0].State; state != StateReady {
		t.Fatalf("an attributable bad result faulted the instance: %s", state)
	}
}

func TestNewRevisionStartsANewGeneration(t *testing.T) {
	f := newSupervisorFixture(t)
	d := f.publish(t, Definition{Name: "repo"})
	first, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	d.Env = map[string]string{"CHANGED": "1"}
	if _, err := f.sup.Publish(t.Context(), d, d.Revision); err != nil {
		t.Fatal(err)
	}
	second, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"})
	if err != nil || instanceOf(t, first) == instanceOf(t, second) {
		t.Fatalf("revision change kept the old process: %s %v", second, err)
	}
	view := f.sup.Services(caller("a"))[0].Instances[0]
	if view.Revision != 2 || view.Generation != 2 {
		t.Fatalf("instance after new revision: %+v", view)
	}
	if _, err := f.sup.Publish(t.Context(), d, d.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
}
