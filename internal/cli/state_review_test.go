package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func reviewControlDial(t *testing.T, respond func(HostRecord, protocol.Control) *protocol.Control) HostDialer {
	t.Helper()
	return func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		a, b := net.Pipe()
		conn, err := transport.NewStreamConn(a)
		if err != nil {
			return nil, fmt.Errorf("review client: %w", err)
		}
		peer, err := transport.NewStreamConn(b)
		if err != nil {
			return nil, fmt.Errorf("review peer: %w", err)
		}
		go func() {
			defer func() { _ = peer.Close() }()
			for {
				frame, err := peer.ReadFrame()
				if err != nil {
					return
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return
				}
				var response *protocol.Control
				if request.Type == protocol.TypeHostInfo {
					response = respond(host, request)
					if response == nil {
						response = &protocol.Control{Type: protocol.TypeHostInfoResult, Host: testHostDeclaration(host)}
					}
				} else {
					response = respond(host, request)
				}
				if response == nil {
					continue
				}
				response.RequestID = request.RequestID
				data, err := response.Encode()
				if err != nil {
					return
				}
				if err := peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}); err != nil {
					return
				}
			}
		}()
		return conn, nil
	}
}
func reviewSession(host HostRecord) protocol.SessionInfo {
	return protocol.SessionInfo{ID: "7K3D", HostID: host.ID, Command: []string{"shell"}, State: "running", CreatedAt: commandTestTime}
}
func reviewSnapshot(host HostRecord) *protocol.Control {
	return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Sessions: []protocol.SessionInfo{reviewSession(host)}, Current: map[string]protocol.Observation{protocol.TopicSessions: {}}}}
}
func TestWatchReviewID5PublishesInitialFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	watcher := NewStateWatcher(func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("fixture offline") })
	var seen StateView
	err := watcher.Watch(ctx, HostRecord{ID: "host", MachineName: "host"}, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, func(v StateView) { seen = v; cancel() })
	if !errors.Is(err, context.Canceled) || !seen.Sections[protocol.TopicSessions].Observation.Failing {
		t.Fatalf("initial failure was not published: %v %+v", err, seen)
	}
}
func TestWatchReviewID8DeepOwnership(t *testing.T) {
	attached := commandTestTime
	exit := 2
	view := StateView{Sessions: []protocol.SessionInfo{{Command: []string{"shell"}, LastAttachedAt: &attached, ExitCode: &exit, Recovery: &recovery.Record{ShellDirectory: "/original", Command: []string{"original"}, Lines: []string{"original"}, Agent: &agentresume.Recipe{Launch: agentresume.Launch{Options: []string{"original"}}}, AgentResume: &agentresume.Receipt{ConversationID: "original"}, Restart: &recovery.Command{Argv: []string{"original"}}, Remote: &recovery.Target{HostID: "original"}}, Hibernated: &recovery.Hibernation{Reason: "original"}}}}
	cloned := view.Clone()
	cloned.Sessions[0].Recovery.ShellDirectory = "/mutation"
	cloned.Sessions[0].Recovery.Command[0] = "mutation"
	cloned.Sessions[0].Recovery.Lines[0] = "mutation"
	cloned.Sessions[0].Recovery.Agent.Options[0] = "mutation"
	cloned.Sessions[0].Recovery.AgentResume.ConversationID = "mutation"
	cloned.Sessions[0].Recovery.Restart.Argv[0] = "mutation"
	cloned.Sessions[0].Recovery.Remote.HostID = "mutation"
	*cloned.Sessions[0].ExitCode = 9
	*cloned.Sessions[0].LastAttachedAt = attached.Add(time.Hour)
	cloned.Sessions[0].Hibernated.Reason = "mutation"
	if view.Sessions[0].Recovery.ShellDirectory != "/original" || view.Sessions[0].Recovery.Command[0] != "original" || *view.Sessions[0].ExitCode != 2 || !view.Sessions[0].LastAttachedAt.Equal(commandTestTime) || view.Sessions[0].Hibernated.Reason != "original" {
		t.Fatal("consumer mutated retained session pointers")
	}
	record := view.Sessions[0].Recovery
	if record.Agent.Options[0] != "original" || record.AgentResume.ConversationID != "original" || record.Restart.Argv[0] != "original" || record.Remote.HostID != "original" {
		t.Fatal("consumer mutated retained nested recovery pointers")
	}
}

type reviewReadContext struct {
	context.Context
	once             sync.Once
	entered, release chan struct{}
}

func (c *reviewReadContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Done()
}
func TestWatchReviewID3OverlappingHostReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	dial := reviewControlDial(t, func(h HostRecord, r protocol.Control) *protocol.Control {
		if r.Type == protocol.TypeStateWatch {
			return reviewSnapshot(h)
		}
		return nil
	})
	state := newPickerState(ctx, dial)
	defer state.close()
	a := HostRecord{ID: "a", MachineName: "a", MeshIdentity: "a"}
	b := HostRecord{ID: "b", MachineName: "b", MeshIdentity: "b"}
	first, err := state.read(ctx, a)
	if err != nil || first.Sessions[0].HostID != a.ID {
		t.Fatal(first, err)
	}
	gate := &reviewReadContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan catalogCollection, 1)
	go func() {
		rows, err := state.read(gate, a)
		result <- catalogCollection{rows: []HostSessions{rows}, err: err}
	}()
	<-gate.entered
	second, err := state.read(ctx, b)
	if err != nil || second.Sessions[0].HostID != b.ID {
		close(gate.release)
		t.Fatal(second, err)
	}
	close(gate.release)
	got := <-result
	if got.err == nil && (len(got.rows[0].Sessions) != 1 || got.rows[0].Sessions[0].HostID != a.ID) {
		t.Fatalf("host A read returned host B rows: %+v", got.rows)
	}
}
func TestWatchReviewID4LegacyTopicTimeoutIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()
	dial := reviewControlDial(t, func(_ HostRecord, r protocol.Control) *protocol.Control {
		switch r.Type {
		case protocol.TypeStateWatch:
			return &protocol.Control{Type: protocol.TypeError, Message: `daemon: unknown control "state.watch"`}
		case protocol.TypeList:
			return nil
		case protocol.TypeServiceList:
			return &protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{{Name: "healthy", Kind: "proxy", Target: "http://127.0.0.1:1"}}}
		}
		return nil
	})
	var got StateView
	watcher := NewStateWatcher(dial)
	_ = watcher.Watch(ctx, HostRecord{ID: "host", MachineName: "host", MeshIdentity: "identity"}, protocol.StateWatch{Topics: []string{protocol.TopicSessions, protocol.TopicServices}}, func(v StateView) {
		if len(v.Services) > 0 {
			got = v
			cancel()
		}
	})
	if len(got.Services) != 1 || !got.Sections[protocol.TopicSessions].Observation.Failing || got.Sections[protocol.TopicServices].Observation.Failing {
		t.Fatalf("sessions timeout starved healthy service topic: %+v", got)
	}
}
func TestWatchReviewID6DurableCatalogAndOfflineReopen(t *testing.T) {
	// Two SQLite opens with migrations under -race exceeded 3s on a loaded CI
	// runner. The bound only stops a hang; a healthy run finishes in about 1s.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	database := filepath.Join(t.TempDir(), "catalog.db")
	store, err := storage.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cache := &SQLiteCatalogCache{store: store, now: time.Now}
	host := HostRecord{ID: "host", MachineName: "host", MeshIdentity: "identity"}
	old := reviewSession(host)
	old.ID = "9ABC"
	if err := cache.Save(ctx, host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	dial := reviewControlDial(t, func(h HostRecord, r protocol.Control) *protocol.Control {
		if r.Type == protocol.TypeStateWatch {
			return reviewSnapshot(h)
		}
		return nil
	})
	state := newPickerState(ctx, dial)
	// Exercise the same cache ownership as command.runPicker without querying session.list.
	state.cache = cache
	rows, err := state.read(ctx, host)
	state.close()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := cache.Load(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].ID != rows.Sessions[0].ID || saved[1].ID != "9ABC" || saved[1].State != "interrupted" {
		t.Fatalf("watch never committed authoritative addition/removal: %+v", saved)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	offlineCache := &SQLiteCatalogCache{store: reopened, now: time.Now}
	offline := newPickerState(ctx, func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("fixture offline") })
	offline.cache = offlineCache
	defer offline.close()
	stale, err := offline.read(ctx, host)
	if err != nil || !stale.Stale || len(stale.Sessions) != 2 || stale.Sessions[0].ID != rows.Sessions[0].ID || stale.Sessions[1].State != "interrupted" {
		t.Fatalf("offline reopen lost durable watch rows: %+v %v", stale, err)
	}
}
func TestWatchReviewID11HealthyStreamResetsBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	var connections atomic.Int32
	dial := func(ctx context.Context, h HostRecord) (transport.Conn, error) {
		connections.Add(1)
		if connections.Load() == 3 {
			cancel()
			return nil, context.Canceled
		}
		a, b := net.Pipe()
		conn, err := transport.NewStreamConn(a)
		if err != nil {
			return nil, fmt.Errorf("review client: %w", err)
		}
		peer, err := transport.NewStreamConn(b)
		if err != nil {
			return nil, fmt.Errorf("review peer: %w", err)
		}
		go func() {
			defer func() { _ = peer.Close() }()
			frame, err := peer.ReadFrame()
			if err != nil {
				return
			}
			request, _ := protocol.DecodeControl(frame.Payload)
			data, _ := (protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity}}).Encode()
			if peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}) != nil {
				return
			}
			frame, err = peer.ReadFrame()
			if err != nil {
				return
			}
			request, _ = protocol.DecodeControl(frame.Payload)
			snap := reviewSnapshot(h)
			snap.RequestID = request.RequestID
			data, _ = snap.Encode()
			if peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}) != nil {
				return
			}
			data, _ = (protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: &protocol.StateCurrent{Seq: 2, Sections: map[string]protocol.Observation{protocol.TopicSessions: {}}}}).Encode()
			_ = peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data})
		}()
		return conn, nil
	}
	_ = NewStateWatcher(dial).Watch(ctx, HostRecord{ID: "host", MachineName: "host", MeshIdentity: "identity"}, protocol.StateWatch{Topics: []string{protocol.TopicSessions}}, func(StateView) {})
	if connections.Load() != 3 {
		t.Fatalf("healthy streams retained increasing backoff: dials=%d", connections.Load())
	}
}

func TestWatchReviewID10ScreenChangeReleasesRemoteDemand(t *testing.T) {
	for _, screen := range []string{"localhost", "overview"} {
		t.Run(screen, func(t *testing.T) {
			fixture := setupCommandTestHost(t)
			var active atomic.Int32
			dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
				a, b := net.Pipe()
				client, err := transport.NewStreamConn(a)
				if err != nil {
					return nil, fmt.Errorf("screen client: %w", err)
				}
				peer, err := transport.NewStreamConn(b)
				if err != nil {
					return nil, fmt.Errorf("screen peer: %w", err)
				}
				go func() {
					watched := false
					defer func() {
						_ = peer.Close()
						if watched {
							active.Add(-1)
						}
					}()
					for {
						frame, err := peer.ReadFrame()
						if err != nil {
							return
						}
						r, err := protocol.DecodeControl(frame.Payload)
						if err != nil {
							return
						}
						response := protocol.Control{RequestID: r.RequestID}
						switch r.Type {
						case protocol.TypeHostInfo:
							response.Type = protocol.TypeHostInfoResult
							response.Host = testHostDeclaration(host)
						case protocol.TypeStateWatch:
							response = *reviewSnapshot(host)
							response.RequestID = r.RequestID
							watched = true
							active.Add(1)
						case protocol.TypeServiceList:
							response.Type = protocol.TypeServiceListed
						case protocol.TypeList:
							response.Type = protocol.TypeListed
							response.Sessions = []protocol.SessionInfo{reviewSession(host)}
						default:
							return
						}
						data, _ := response.Encode()
						if peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: data}) != nil {
							return
						}
					}
				}()
				return client, nil
			}
			_, _, err := executeCommand(t, Dependencies{DialControl: dial, Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
				snapshot, err := input.Refresh(ctx, fixture.host.ID)
				if err != nil || snapshot.Sessions.Stale || active.Load() != 1 {
					t.Fatal(snapshot, err, active.Load())
				}
				if screen == "localhost" {
					_, err = input.Refresh(ctx, localHostID())
				} else {
					_, err = input.LoadHosts(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.NewTimer(200 * time.Millisecond)
				defer deadline.Stop()
				for active.Load() != 0 {
					select {
					case <-deadline.C:
						t.Error("hidden remote screen retained its watch demand")
						return PickerSelection{}, nil
					default:
						time.Sleep(time.Millisecond)
					}
				}
				return PickerSelection{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWatchReviewID4UnsupportedOnlyBuildChange(t *testing.T) {
	host := HostRecord{ID: "host", MachineName: "host", MeshIdentity: "identity"}
	var version atomic.Int32
	var probes atomic.Int32
	dial := reviewControlDial(t, func(h HostRecord, r protocol.Control) *protocol.Control {
		if r.Type == protocol.TypeHostInfo {
			return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, Build: &release.Build{Version: fmt.Sprint(version.Load())}}}
		}
		probes.Add(1)
		return &protocol.Control{Type: protocol.TypeError, Message: `daemon: unknown control "host.metrics"`}
	})
	watcher := NewStateWatcher(dial)
	build, err := json.Marshal(&release.Build{Version: "0"})
	if err != nil {
		t.Fatal(err)
	}
	section := pollSection{topic: protocol.TopicMetrics, capability: "host/identity/host.metrics", build: string(build)}
	view := StateView{Sections: map[string]ObservedSection{}}
	request := protocol.StateWatch{Topics: []string{protocol.TopicMetrics}}
	if _, err := watcher.pollDue(t.Context(), host, request, &section, &view, func(StateView) {}); err != nil {
		t.Fatal(err)
	}
	if !section.unsupported || probes.Load() != 1 {
		t.Fatalf("known-good explicit unknown control missing: %+v probes=%d", section, probes.Load())
	}
	version.Add(1)
	section.due = time.Time{}
	if _, err := watcher.pollDue(t.Context(), host, request, &section, &view, func(StateView) {}); !errors.Is(err, errStateBuildChanged) {
		t.Fatalf("unsupported-only polling concealed changed observed build: %v", err)
	}
	if probes.Load() != 1 {
		t.Fatal("unsupported control was repeated before build reprobe")
	}
}

func TestWatchReviewID3SameHostChangedIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var watches atomic.Int32
	dial := reviewControlDial(t, func(h HostRecord, r protocol.Control) *protocol.Control {
		if r.Type != protocol.TypeStateWatch {
			return nil
		}
		watches.Add(1)
		response := reviewSnapshot(h)
		response.StateSnapshot.Sessions[0].Label = h.MeshIdentity
		return response
	})
	state := newPickerState(ctx, dial)
	defer state.close()
	host := HostRecord{ID: "host", MachineName: "host", Endpoint: "same", MeshIdentity: "first"}
	first, err := state.read(ctx, host)
	if err != nil || first.Sessions[0].Label != "first" {
		t.Fatal(first, err)
	}
	host.MeshIdentity = "changed"
	next, err := state.read(ctx, host)
	if err != nil || next.Sessions[0].Label != "changed" || watches.Load() != 2 {
		t.Fatalf("changed identity reused old generation: %+v watches=%d error=%v", next, watches.Load(), err)
	}
}
