package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestWatchReviewID1IndependentRecoveryRefresh(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	broker := newStateBroker(2, func() time.Time { return now })
	row := storage.Session{ID: "7K3D", HostID: "host", Command: []string{"shell"}, Cwd: root}
	record := recovery.Record{Version: recovery.Version, HostID: "host", SessionID: "7K3D", CheckpointAt: now, Shell: "sh", ShellDirectory: root, DirectorySource: recovery.DirectoryLaunch, Command: []string{"shell"}}
	dir := filepath.Join(root, "7K3D")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	lifecycle := &lifecycle{sessionsDir: root, host: storage.Host{ID: "host"}}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{row}})
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	broker.projectDirty(t.Context(), lifecycle)
	initial := broker.snapshotLockedForTest(sub)
	if initial.Sessions[0].Recovery == nil || initial.Sessions[0].Recovery.ShellDirectory != root {
		t.Fatalf("known-good projection absent: %+v", initial.Sessions)
	}
	_ = broker.take(sub)
	record.ShellDirectory = filepath.Join(root, "changed")
	record.CheckpointAt = now.Add(time.Second)
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	broker.sessionsChanged(SessionDiff{MetadataChanged: []storage.SessionID{row.ID}})
	broker.projectDirty(t.Context(), lifecycle)
	next := broker.snapshotLockedForTest(sub)
	if next.Sessions[0].Recovery.ShellDirectory != record.ShellDirectory {
		t.Fatal("unchanged committed row concealed changed recovery file")
	}
	events := broker.take(sub)
	if len(events) != 1 || events[0].StateEvent == nil {
		t.Fatalf("metadata change did not reach existing subscriber: %+v", events)
	}
	second, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(second)
	if snapshot.Sessions[0].Recovery.ShellDirectory != record.ShellDirectory {
		t.Fatal("new subscriber saw old recognition")
	}
}
func TestWatchReviewID2RetiringRouteMatchesCatalog(t *testing.T) {
	_, _, controller := newServiceControllerTest(t, "/control")
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return true })
	manager.cleanupRetry = time.Hour
	controller.demand = manager
	broker := newStateBroker(1, time.Now)
	controller.onCommitted = broker.servicesCommitted
	manager.onChange = broker.serviceDemandChanged
	manager.Sync([]meshserve.Service{demandService(time.Minute)})
	if err := manager.Start(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	sessions.failStops(errWorkerSilent)
	manager.Sync(nil)
	waitForSettledRoute(t, manager)
	controller.publishCommitted()
	sub, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicServices}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	listed, err := controller.list(t.Context(), protocol.Control{Type: protocol.TypeServiceList, RequestID: "retiring"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Services) != 1 || len(snapshot.Services) != 1 || snapshot.Services[0].Demand.SessionID != listed.Services[0].Demand.SessionID {
		t.Fatalf("watch lost still-owned retiring route: watch=%+v list=%+v", snapshot.Services, listed.Services)
	}
	sessions.failStops(nil)
	manager.mu.Lock()
	route := manager.retiring["dev"]
	manager.mu.Unlock()
	route.mu.Lock()
	route.retryAt = time.Time{}
	route.mu.Unlock()
	manager.supervise()
	waitForSettledRoute(t, manager)
	manager.supervise()
	next := broker.snapshotLockedForTest(sub)
	if len(next.Services) != 0 {
		t.Fatalf("completed retirement remained in watch: %+v", next.Services)
	}
}

type reviewWriterGate struct {
	transport.Conn
	entered, release, concurrent chan struct{}
	once                         sync.Once
	writing                      atomic.Bool
}

func (c *reviewWriterGate) WriteFrame(frame protocol.Frame) error {
	if c.writing.Swap(true) {
		c.once.Do(func() { close(c.concurrent) })
	}
	defer c.writing.Store(false)
	message, _ := protocol.DecodeControl(frame.Payload)
	if message.Type == protocol.TypeHostInfoResult {
		close(c.entered)
		<-c.release
	}
	if err := c.Conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("review write: %w", err)
	}
	return nil
}
func TestWatchReviewID9PipelinedWriterHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	a, b := net.Pipe()
	client, err := transport.NewStreamConn(a)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := transport.NewStreamConn(b)
	if err != nil {
		t.Fatal(err)
	}
	gate := &reviewWriterGate{Conn: peer, entered: make(chan struct{}), release: make(chan struct{}), concurrent: make(chan struct{})}
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.state = newStateBroker(1, time.Now)
	server.metrics = hostmetrics.New()
	done := make(chan error, 1)
	go func() { done <- server.Handle(ctx, gate) }()
	defer func() { _ = client.Close(); <-done }()
	if err := client.WriteFrame(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "info"})); err != nil {
		t.Fatal(err)
	}
	<-gate.entered
	if err := client.WriteFrame(serverControlFrame(t, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "watch", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicSessions}}})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.concurrent:
		t.Error("watch wrote concurrently with queued ordinary reply")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate.release)
	first, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	one, two := decodeServerControl(t, first), decodeServerControl(t, second)
	if one.RequestID != "info" || two.RequestID != "watch" || two.Type != protocol.TypeStateSnapshot {
		t.Fatalf("pipeline reply order changed: first=%+v second=%+v", one, two)
	}
}

func TestWatchReviewID1MetadataRevisionNoIdleProjection(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", "running", "boot-a")
	writeCatalogMeta(t, root, meta.ID, meta)
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	broker := newStateBroker(2, func() time.Time { return now })
	catalog.onChange, catalog.onObservation = broker.sessionsChanged, broker.observeSessions
	dir := filepath.Join(root, meta.ID)
	record := recovery.Record{Version: recovery.Version, HostID: string(catalog.host.ID), SessionID: meta.ID, CheckpointAt: now, Shell: "sh", ShellDirectory: root, DirectorySource: recovery.DirectoryLaunch, Command: []string{"shell"}}
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	owner := &lifecycle{sessionsDir: root, host: catalog.host}
	broker.projectDirty(t.Context(), owner)
	if broker.snapshotLockedForTest(sub).Sessions[0].Recovery.ShellDirectory != root {
		t.Fatal("known-good metadata projection missing")
	}
	writes := store.reconcileCalls
	now = now.Add(30 * time.Second)
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if row, dirty := broker.nextProjection(); dirty {
		t.Fatalf("unchanged metadata scheduled full recovery work: %s", row.stored.ID)
	}
	record.ShellDirectory = filepath.Join(root, "changed")
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	broker.projectDirty(t.Context(), owner)
	if broker.snapshotLockedForTest(sub).Sessions[0].Recovery.ShellDirectory != record.ShellDirectory {
		t.Fatal("metadata-only revision missed projection")
	}
	if store.reconcileCalls != writes {
		t.Fatal("metadata-only revision persisted durable worker rows")
	}
}

func TestWatchReviewID1MetadataSupersededCompletion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{Version: recovery.Version, HostID: "host", SessionID: "7K3D", CheckpointAt: time.Now(), Shell: "sh", ShellDirectory: root, DirectorySource: recovery.DirectoryLaunch, Command: []string{"shell"}}
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(2, time.Now)
	row := storage.Session{ID: "7K3D", HostID: "host", Command: []string{"shell"}, Cwd: root}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{row}})
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	owner := &lifecycle{sessionsDir: root, host: storage.Host{ID: "host"}}
	projection, _ := broker.nextProjection()
	read, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		info := sessionInfo(projection.stored)
		owner.addRecoveryInfo(&info, true)
		close(read)
		<-release
		broker.commitProjection(projection, info)
		close(done)
	}()
	<-read
	record.ShellDirectory = filepath.Join(root, "new")
	if err := recovery.Write(dir, record); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	broker.sessionsChanged(SessionDiff{MetadataChanged: []storage.SessionID{row.ID}})
	broker.projectDirty(t.Context(), owner)
	close(release)
	<-done
	if broker.snapshotLockedForTest(sub).Sessions[0].Recovery.ShellDirectory != record.ShellDirectory {
		t.Fatal("old recognition completion replaced newer metadata revision")
	}
}

func TestWatchReviewID1SourceMetadataInvalidatesDependentRecognition(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	source := storage.Session{ID: "7K3D"}
	dependent := storage.Session{ID: "9ABC"}
	unrelated := storage.Session{ID: "Q8ME"}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{source, dependent, unrelated}})
	clear(broker.dirty)
	broker.sessions["9ABC"] = protocol.SessionInfo{ID: "9ABC", RecoveredFrom: "7K3D"}
	broker.sessionsChanged(SessionDiff{MetadataChanged: []storage.SessionID{source.ID}})
	if !broker.dirty[source.ID] || !broker.dirty[dependent.ID] || broker.dirty[unrelated.ID] {
		t.Fatalf("metadata dependency invalidation = %v", broker.dirty)
	}
}

func TestWatchReviewID1CheckpointTimeCarriesWithoutEvent(t *testing.T) {
	broker := newStateBroker(1, time.Now)
	row := storage.Session{ID: "7K3D"}
	broker.sessionsChanged(SessionDiff{Added: []storage.Session{row}})
	sub, _, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicSessions}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	info := sessionInfo(row)
	info.Recovery = &recovery.Record{CheckpointAt: time.Now()}
	first, _ := broker.nextProjection()
	broker.commitProjection(first, info)
	_ = broker.take(sub)
	broker.sessionsChanged(SessionDiff{MetadataChanged: []storage.SessionID{row.ID}})
	next, _ := broker.nextProjection()
	latest := *info.Recovery
	latest.CheckpointAt = latest.CheckpointAt.Add(time.Second)
	info.Recovery = &latest
	broker.commitProjection(next, info)
	if events := broker.take(sub); len(events) != 0 {
		t.Fatalf("checkpoint-only change published session event: %+v", events)
	}
	if !broker.snapshotLockedForTest(sub).Sessions[0].Recovery.CheckpointAt.Equal(latest.CheckpointAt) {
		t.Fatal("latest checkpoint value was discarded")
	}
	broker.sessionsChanged(SessionDiff{MetadataChanged: []storage.SessionID{row.ID}})
	activity, _ := broker.nextProjection()
	activityRecord := latest
	activityRecord.LastOutputAt = latest.CheckpointAt
	activityInfo := info
	activityInfo.Recovery = &activityRecord
	broker.commitProjection(activity, activityInfo)
	if events := broker.take(sub); len(events) != 1 {
		t.Fatalf("activity observation did not publish: %+v", events)
	}
}
