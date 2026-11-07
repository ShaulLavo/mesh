package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/worker"
)

const disclosureSentinel = "OUTPUT-PATH-RECIPE-SECRET-DO-NOT-DISCLOSE"

type scopedServiceStore struct {
	reads  atomic.Int32
	writes atomic.Int32
	rows   []meshserve.Service
}

func (s *scopedServiceStore) ListServices(context.Context) ([]meshserve.Service, error) {
	s.reads.Add(1)
	return s.rows, nil
}
func (s *scopedServiceStore) UpsertService(context.Context, meshserve.Service) (meshserve.Service, error) {
	s.writes.Add(1)
	return meshserve.Service{}, errors.New("unexpected service write")
}
func (s *scopedServiceStore) DeleteService(context.Context, string) error {
	s.writes.Add(1)
	return errors.New("unexpected service removal")
}

type scopedCatalog struct {
	reads     atomic.Int32
	mutations atomic.Int32
	rows      []storage.Session
	entered   chan struct{}
	release   <-chan struct{}
}

func (c *scopedCatalog) List(ctx context.Context) ([]storage.Session, error) {
	c.reads.Add(1)
	if c.entered != nil {
		close(c.entered)
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, fmt.Errorf("fixture catalog wait: %w", ctx.Err())
		}
	}
	return c.rows, nil
}
func (c *scopedCatalog) Get(ctx context.Context, id storage.SessionID) (storage.Session, error) {
	rows, err := c.List(ctx)
	if err != nil {
		return storage.Session{}, err
	}
	for _, row := range rows {
		if row.ID == id {
			return row, nil
		}
	}
	return storage.Session{}, errAccessNotFound
}
func (c *scopedCatalog) Reconcile(context.Context) error { c.mutations.Add(1); return nil }
func (c *scopedCatalog) Remove(context.Context, storage.SessionID) error {
	c.mutations.Add(1)
	return nil
}
func (c *scopedCatalog) PruneLabelled(context.Context, string, storage.SessionID) ([]storage.SessionID, error) {
	c.mutations.Add(1)
	return nil, nil
}

type scopedFixture struct {
	state, target, subject string
	key                    ed25519.PrivateKey
	store                  *access.Store
	auth                   *transport.Authentication
	server                 *clientServer
	catalog                *scopedCatalog
	services               *scopedServiceStore
	workers                contextWorkerCounter
	owners                 contextControlCounter
	launches               atomic.Int32
	privateReads           atomic.Int32
	http                   *httptest.Server
	ctx                    context.Context
	cancel                 context.CancelFunc
	joined                 sync.WaitGroup
}

func newScopedFixture(ctx context.Context, t *testing.T, loose bool) *scopedFixture {
	t.Helper()
	owned := newDaemonRunFixture(t)
	f := &scopedFixture{state: owned.stateDir}
	lifetime, cancel := context.WithCancel(ctx)
	f.ctx, f.cancel = lifetime, cancel
	t.Cleanup(func() {
		f.cancel()
		if f.http != nil {
			f.http.Close()
		}
		done := make(chan struct{})
		go func() { f.joined.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("scoped fixture handlers did not join")
		}
	})
	host, _, err := identity.LoadOrCreate(f.state)
	if err != nil {
		t.Fatal(err)
	}
	subject, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.target, f.subject, f.key = host.ID, subject.ID, key
	f.store, err = access.Open(f.state, f.target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Grant(lifetime, 0, access.GrantInput{Permissions: []string{"metadata"}, Subject: access.Subject(f.subject), ExpiresAt: time.Now().Add(time.Hour)}, observeOtherRoles(f.state)); err != nil {
		t.Fatal(err)
	}
	f.catalog = &scopedCatalog{rows: []storage.Session{{ID: "7K3D", HostID: storage.HostID(f.target), Command: []string{disclosureSentinel}, Cwd: disclosureSentinel, State: storage.StateDetached, CreatedAt: time.Now().UTC(), LastOutputSequence: 991}}}
	f.services = &scopedServiceStore{rows: []meshserve.Service{{Name: "site", Kind: meshserve.Proxy, Target: disclosureSentinel, DisplayName: disclosureSentinel}}}
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	serviceController, err := newServiceController(lifetime, t.TempDir(), f.services, registry)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := mustLifecycle(t, lifecycleConfig{Catalog: f.catalog, Connector: &f.workers, Host: storage.Host{ID: storage.HostID(f.target), MeshIdentity: f.target, TailscaleName: pointerString(disclosureSentinel)}, SessionsDir: filepath.Join(f.state, "s"), Launch: func(worker.LaunchConfig) (worker.Launched, error) {
		f.launches.Add(1)
		return worker.Launched{}, errors.New("unexpected launch")
	}, PrivateName: func() string { f.privateReads.Add(1); return disclosureSentinel }})
	f.server, err = newClientServer(lifecycle, &f.workers, serviceController, &f.owners)
	if err != nil {
		t.Fatal(err)
	}
	f.server.apps, f.server.fleet, f.server.context, f.server.updates = &f.owners, &f.owners, &f.owners, &f.owners
	f.auth, err = controlAuthentication(f.state)
	if err != nil {
		t.Fatal(err)
	}
	if loose {
		native := f.auth.Bind
		admit := func(protocol.Frame) bool { return true }
		f.auth.Bind = func(id string) transport.Authorization {
			grant := native(id)
			grant.Admit = admit
			return grant
		}
	}
	f.http = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.joined.Add(1)
		defer f.joined.Done()
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: f.auth}, func(ctx context.Context, conn transport.Conn) error {
			return f.handleScopedConnection(ctx, lifetime, conn)
		})
	}))
	f.http.Start()
	return f
}
func pointerString(value string) *string { return &value }
func (f *scopedFixture) dial(t *testing.T, admission transport.Admission) (transport.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, f.http.URL, transport.DialOptions{Auth: &transport.Authentication{Key: f.key, ExpectedIdentity: f.target, Admission: admission}})
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	if err != nil {
		return nil, fmt.Errorf("fixture scoped dial: %w", err)
	}
	return conn, nil
}
func (f *scopedFixture) read(t *testing.T, read protocol.AccessRead) protocol.AccessReply {
	t.Helper()
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return readScoped(t, conn, read)
}
func readScoped(t *testing.T, conn transport.Conn, read protocol.AccessRead) protocol.AccessReply {
	t.Helper()
	data, err := json.Marshal(protocol.AccessRequest{Type: protocol.TypeAccessRead, RequestID: "scope-read", Read: read})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame.Payload, []byte(disclosureSentinel)) {
		t.Fatalf("protected data disclosed: %s", frame.Payload)
	}
	reply, err := protocol.DecodeAccessReply(frame.Payload)
	if err != nil {
		t.Fatal(err, string(frame.Payload))
	}
	return reply
}
func TestScopedRealTLSProjectsOnlyAllowedMetadata(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	for _, section := range []string{"host", "sessions", "services"} {
		reply := f.read(t, protocol.AccessRead{Section: section})
		if reply.Result == nil || reply.Result.Subject.Principal != f.subject || reply.Result.Subject.TargetID != f.target || reply.Result.Subject.Class != "scoped" {
			t.Fatalf("binding missing: %+v", reply)
		}
	}
	if f.catalog.reads.Load() != 1 || f.services.reads.Load() != 1 || f.workers.calls.Load() != 0 || f.launches.Load() != 0 || f.owners.calls.Load() != 0 || f.privateReads.Load() != 0 || f.services.writes.Load() != 0 {
		t.Fatal("metadata path invoked forbidden owner work")
	}
	if _, err := os.Stat(filepath.Join(f.state, "s")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata initialized session payload: %v", err)
	}
	for _, admission := range []transport.Admission{transport.FullAdmission, transport.SignedUpdateAdmission} {
		if conn, err := f.dial(t, admission); err == nil {
			_ = conn.Close()
			t.Fatal("ordinary/updater client accepted a scoped key")
		}
	}
}

var deniedScopedTypes = []string{
	"execution.create", "execution.request", "execution.receipt",
	"host.metrics.result", "session.created", "session.listed", "host.info.result", "host.renamed", "session.recovered", "session.recovery-record", "copy.result", "app.result", "host.context.result", "service.previewed", "service.upserted", "service.listed", "service.deleted", "service.labeled", "certificate.installed", "wake.remembered", "wake.probed", "wake.sent", "wake.configured",
	"host.info", "host.metrics", "host.context", "host.rename", "session.list", "session.inspect", "session.logs", "session.recovery", "session.create", "session.attach", "session.attach-detached", "terminal.resize", "session.detach", "session.signal", "session.kill", "session.hibernate", "session.recover", "session.recovery-command", "session.remove", "service.preview", "service.upsert", "service.label", "service.list", "service.delete", "service.start", "service.stop", "fleet.hosts", "certificate.install", "wake.configure", "wake.remember", "wake.probe", "wake.send", "state.watch", "catalog.continue", "session.agent-begin", "session.agent-event", "session.agent-finish", "session.shell-update", "session.containment", "session.nest", "session.attached", "session.nesting", "session.exit", "session.logged", "session.inspected", "state.snapshot", "state.event", "state.current", "state.resync", "error", "ok", "access.output.result", "future.control",
}
var deniedScopedActions = map[string][]string{
	"copy.request":   {"upload.begin", "upload.chunk", "upload.commit", "download.begin", "download.chunk", "close", "release"},
	"app.request":    {"upload.begin", "upload.chunk", "create", "update", "download", "list", "renew", "browser.inspect", "browser.list", "browser.approve", "browser.revoke", "inspect", "delete"},
	"app.registry":   {"allocate", "list", "sync", "inspect", "activate", "suspend", "renew", "delete", "cleanup", "browser.inspect", "browser.list", "browser.approve", "browser.revoke"},
	"update.control": {"challenge", "info", "runs", "status", "plan", "retry", "cancel", "stage", "grant", "install-status", "install-cancel", "install-retry", "fleet-profile.put", "automatic.status", "automatic.consent", "automatic.change", "unknown"},
}

func TestScopedFramedDenialMatrixStopsBeforeAllOwners(t *testing.T) {
	for _, loose := range []bool{false, true} {
		t.Run(fmt.Sprint(loose), func(t *testing.T) { denyScopedFrames(t, loose) })
	}
}
func denyScopedFrames(t *testing.T, loose bool) {
	t.Helper()
	f := newScopedFixture(t.Context(), t, loose)
	for _, name := range deniedScopedTypes {
		assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: []byte(fmt.Sprintf(`{"type":%q,"requestId":"denied","sessionId":"7K3D","cols":1,"rows":1,"lean":true}`, name))})
	}
	for parent, actions := range deniedScopedActions {
		for _, action := range actions {
			assertScopedDenied(t, f, scopedActionFrame(t, parent, action))
		}
	}
	for _, kind := range []protocol.Kind{protocol.KindInput, protocol.KindData, protocol.KindSnapshot, 0xff} {
		assertScopedDenied(t, f, protocol.Frame{Kind: kind, Payload: []byte(disclosureSentinel)})
	}
	if f.catalog.reads.Load() != 0 || f.services.reads.Load() != 0 || f.services.writes.Load() != 0 || f.workers.calls.Load() != 0 || f.launches.Load() != 0 || f.owners.calls.Load() != 0 || f.privateReads.Load() != 0 {
		t.Fatal("denied frame reached an owner")
	}
	if f.catalog.mutations.Load() != 0 {
		t.Fatal("denied frame mutated catalog")
	}
	if _, err := os.Stat(filepath.Join(f.state, "s")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied frame created payload: %v", err)
	}
}
func scopedActionFrame(t *testing.T, parent, action string) protocol.Frame {
	t.Helper()
	field := "app"
	if parent == "copy.request" {
		field = "copy"
	}
	if parent == "update.control" {
		field = "update"
	}
	data, err := json.Marshal(map[string]any{"type": parent, "requestId": "denied", field: map[string]any{"action": action, "actor": "forged", "principal": "local-account", "signature": "untrusted", "path": disclosureSentinel}})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: data}
}
func assertScopedDenied(t *testing.T, f *scopedFixture, frame protocol.Frame) {
	t.Helper()
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.WriteFrame(frame)
	got, err := conn.ReadFrame()
	if err == nil {
		t.Fatalf("denied request received bytes %q for %q", got.Payload, frame.Payload)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("denied stream did not retire before deadline", err)
	}
}
func TestScopedPagesBindEveryCursorField(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	f.catalog.rows = append(f.catalog.rows, storage.Session{ID: "ABCD", HostID: storage.HostID(f.target), State: storage.StateExited, CreatedAt: time.Now().UTC()})
	first := f.read(t, protocol.AccessRead{Section: "sessions", Limit: 1}).Result
	if first == nil || first.NextPage == nil || len(first.Sessions) != 1 || first.Sessions[0].ID != "7K3D" {
		t.Fatal("first page", first)
	}
	second := f.read(t, protocol.AccessRead{Section: "sessions", Limit: 1, Cursor: *first.NextPage}).Result
	if second == nil || len(second.Sessions) != 1 || second.Sessions[0].ID != "ABCD" || second.NextPage != nil {
		t.Fatal("second page", second)
	}
	cursor, err := protocol.DecodeAccessCursor(*first.NextPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"target", "subject", "incarnation", "section", "sessionId"} {
		changed := cursor
		switch field {
		case "target":
			changed.Target = "other"
		case "subject":
			changed.Subject = "other"
		case "incarnation":
			changed.Incarnation = "other"
		case "section":
			changed.Section = "services"
		case "sessionId":
			changed.SessionID = "ABCD"
		}
		token, err := changed.Encode()
		if err != nil {
			t.Fatal(err)
		}
		reply := f.read(t, protocol.AccessRead{Section: "sessions", Cursor: token})
		if reply.Result != nil || reply.ErrorCode != "usage.invalid" {
			t.Fatal("cursor mismatch admitted", field)
		}
	}
}
func TestScopedIdleRevocationAndRegrantCannotHeal(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() { _, err := conn.ReadFrame(); received <- err }()
	if _, err := f.store.Revoke(t.Context(), 1, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-received:
		if err == nil {
			t.Fatal("idle socket returned protected data")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle revocation did not close socket")
	}
	if _, err := f.store.Grant(t.Context(), 2, access.GrantInput{Permissions: []string{"metadata"}, Subject: access.Subject(f.subject), ExpiresAt: time.Now().Add(time.Hour)}, observeOtherRoles(f.state)); err != nil {
		t.Fatal(err)
	}
	if f.read(t, protocol.AccessRead{Section: "host"}).Result == nil {
		t.Fatal("new binding unavailable")
	}
}
func TestScopedReadCompletedAfterRevokeDoesNotReply(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	release := make(chan struct{})
	defer close(release)
	f.catalog.entered = make(chan struct{})
	f.catalog.release = release
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"type":"access.read","requestId":"queued","accessRead":{"section":"sessions"}}`)
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.catalog.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog read did not enter")
	}
	if _, err := f.store.Revoke(t.Context(), 1, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() { _, err := conn.ReadFrame(); returned <- err }()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("revoked read disclosed a reply")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked read did not cancel")
	}
}
func TestScopedRawMalformedFieldsNeverReachCatalog(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	valid := `{"type":"access.read","requestId":"raw","accessRead":{"section":"sessions"}}`
	for _, data := range []string{
		strings.Replace(valid, `"section":"sessions"`, `"section":"sessions","section":"host"`, 1),
		strings.Replace(valid, `"accessRead":`, `"actor":"local-account","accessRead":`, 1),
		strings.Replace(valid, `"accessRead":`, `"recovery":{"lines":["secret"]},"accessRead":`, 1),
		strings.Replace(valid, `"section":"sessions"`, `"section":"host","limit":0`, 1),
		"{\xff}", strings.Repeat("x", protocol.MaximumAccessRequest+1),
	} {
		assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: []byte(data)})
	}
	if f.catalog.reads.Load() != 0 {
		t.Fatal("malformed request reached catalog")
	}
}
func TestMissingAcceptedContextCannotReachGenericDispatch(t *testing.T) {
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Handle(t.Context(), newServerTestConn()); !errors.Is(err, access.ErrDenied) {
		t.Fatal("missing context admitted", err)
	}
}

func TestScopedCarrierCannotUseIndependentValidSignatures(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(t.Context(), f.state, actor.ID, false); err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	authority := &update.Authority{StateDir: f.state, ID: f.target, Key: f.auth.Key, Handle: func(context.Context, update.VerifiedActor, string, json.RawMessage) (any, error) {
		effects.Add(1)
		return map[string]bool{"accepted": true}, nil
	}}
	f.server.updates = authority
	challenge := update.Message{Action: "challenge", Actor: actor.ID, Target: f.target}
	challenge.Sign(key)
	challengeData, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	response, _, err := authority.HandleControl(t.Context(), protocol.Control{Type: update.ControlType, RequestID: "challenge", Update: challengeData})
	if err != nil {
		t.Fatal(err)
	}
	var offer update.Message
	if err := json.Unmarshal(response.Update, &offer); err != nil {
		t.Fatal(err)
	}
	if err := offer.Verify(f.target); err != nil {
		t.Fatal(err)
	}
	var nonce string
	if err := json.Unmarshal(offer.Data, &nonce); err != nil {
		t.Fatal(err)
	}
	message := update.Message{Action: "info", Actor: actor.ID, Target: f.target, Nonce: nonce}
	message.Sign(key)
	if err := message.Verify(actor.ID); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	control := protocol.Control{Type: update.ControlType, RequestID: "signed", Update: data}
	raw, err := control.Encode()
	if err != nil {
		t.Fatal(err)
	}
	assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: raw})
	if effects.Load() != 0 {
		t.Fatal("scoped carrier reached independently authorized update owner")
	}
	reply, _, err := authority.HandleControl(t.Context(), control)
	if err != nil {
		t.Fatal("denied carrier consumed valid nonce", err)
	}
	var accepted update.Message
	if err := json.Unmarshal(reply.Update, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Problem != "" {
		t.Fatal("valid signed action rejected", accepted.Problem)
	}
	if effects.Load() != 1 {
		t.Fatal("existing independent signed actor path changed")
	}
	edge := &testAppEdge{}
	f.server.apps = &appController{registry: edge}
	signed, err := apps.Sign("mesh-app/request/v1", f.target, 1, apps.Request{Action: "list"}, key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.Verify("mesh-app/request/v1", f.target, actor.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	appData, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = (protocol.Control{Type: protocol.TypeAppRegistry, RequestID: "signed-app", App: appData}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: raw})
	if edge.calls != 0 {
		t.Fatal("independent app-owner signature promoted scoped carrier")
	}
}

func TestScopedQueuedReadRechecksBeforeCatalogAdmission(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	reader := f.server.access
	for range cap(reader.slots) {
		reader.slots <- struct{}{}
	}
	defer func() {
		for len(reader.slots) > 0 {
			<-reader.slots
		}
	}()
	binding, err := f.store.Bind(access.Subject(f.subject))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	binding = binding.Constrain(func() bool { once.Do(func() { close(entered) }); return true })
	done := make(chan error, 1)
	go func() { _, err := reader.Read(f.ctx, binding, protocol.AccessRead{Section: "sessions"}); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("read did not reach admission")
	}
	if _, err := f.store.Revoke(t.Context(), 1, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	<-reader.slots
	select {
	case err := <-done:
		if !errors.Is(err, access.ErrDenied) {
			t.Fatal("queued read admitted after revoke", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued read did not finish")
	}
	if f.catalog.reads.Load() != 0 {
		t.Fatal("revoked queued read reached catalog")
	}
}

func TestScopedReplyRejectsDamagedEnumsWithoutDisclosingStoredText(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	f.catalog.rows[0].State = storage.SessionState(disclosureSentinel)
	reply := f.read(t, protocol.AccessRead{Section: "sessions"})
	if reply.Result != nil || reply.ErrorCode != "state.unavailable" {
		t.Fatal("invalid session state was disclosed", reply)
	}
	f.services.rows[0].Kind = meshserve.Kind(disclosureSentinel)
	reply = f.read(t, protocol.AccessRead{Section: "services"})
	if reply.Result != nil || reply.ErrorCode != "state.unavailable" {
		t.Fatal("invalid service kind was disclosed", reply)
	}
}
func TestScopedPagesRespectEncodedByteCeiling(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	f.services.rows = make([]meshserve.Service, 500)
	for index := range f.services.rows {
		f.services.rows[index] = meshserve.Service{Name: strings.Repeat("<", 180) + fmt.Sprintf("%03d", index), Kind: meshserve.Proxy}
	}
	reply := f.read(t, protocol.AccessRead{Section: "services", Limit: 500})
	if reply.Result == nil || len(reply.Result.Services) == 0 || len(reply.Result.Services) >= 500 || reply.Result.NextPage == nil {
		t.Fatal("byte-limited page was not bounded", reply)
	}
	raw, err := reply.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > protocol.MaximumAccessReply {
		t.Fatal("page exceeded encoded byte ceiling")
	}
	next := f.read(t, protocol.AccessRead{Section: "services", Limit: 500, Cursor: *reply.Result.NextPage})
	if next.Result == nil || len(next.Result.Services) == 0 {
		t.Fatal("byte-limited cursor did not continue")
	}
}

func observeOtherRoles(stateDir string) access.ObserveOtherRoles {
	return func(ctx context.Context, subject access.Subject) (access.OtherRoles, error) {
		if err := ctx.Err(); err != nil {
			return access.OtherRoles{}, fmt.Errorf("observe key classes: %w", err)
		}
		return publishedOtherRoles(stateDir, subject)

	}
}

func (f *scopedFixture) handleScopedConnection(ctx, lifetime context.Context, conn transport.Conn) error {
	current, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(lifetime, func() { cancel(); _ = conn.Close() })
	defer stop()
	return f.server.Handle(current, conn)
}

func TestScopedExecutionRequestsStopBeforeOwners(t *testing.T) {
	for _, loose := range []bool{false, true} {
		t.Run(fmt.Sprint(loose), func(t *testing.T) { denyScopedExecutionRequests(t, loose) })
	}
}

func denyScopedExecutionRequests(t *testing.T, loose bool) {
	t.Helper()
	f := newScopedFixture(t.Context(), t, loose)
	requests := []protocol.Control{
		{Type: protocol.TypeExecutionCreate, RequestID: "create-denied", RequestKey: "scoped-job", ExecutionKind: "pty", Command: []string{"/bin/true"}, Cwd: "/work"},
		{Type: protocol.TypeExecutionRequest, RequestID: "lookup-denied", RequestKey: "scoped-job"},
	}
	for _, request := range requests {
		data, err := request.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := protocol.DecodeControl(data); err != nil {
			t.Fatal("execution fixture must be valid for full admission", err)
		}
		assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: data})
	}
	if f.catalog.reads.Load() != 0 || f.catalog.mutations.Load() != 0 || f.launches.Load() != 0 || f.workers.calls.Load() != 0 || f.owners.calls.Load() != 0 || f.services.reads.Load() != 0 || f.services.writes.Load() != 0 || f.privateReads.Load() != 0 {
		t.Fatal("denied execution reached an owner")
	}
	if f.server.lifecycle.execution != nil {
		t.Fatal("denied execution opened the journal")
	}
	for _, name := range []string{"execution", "s"} {
		if _, err := os.Lstat(filepath.Join(f.state, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("denied execution created %s state: %v", name, err)
		}
	}
}
