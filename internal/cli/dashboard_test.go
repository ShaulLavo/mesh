package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestDashboardProjectionBoundsTotalsAndIndependentAges(t *testing.T) {
	now := time.Now()
	state := StateView{Connection: StateReachable, LastReply: now, MetricsReceivedAt: now, ServiceHealthSupported: true,
		Sections: map[string]ObservedSection{protocol.TopicSessions: {ReceivedAt: now}, protocol.TopicServices: {ReceivedAt: now}},
		Metrics:  &hostmetrics.Snapshot{CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Available, Value: 0, Sample: "cpu", AgeMillis: 100}, RAM: hostmetrics.Reading[hostmetrics.Memory]{Availability: hostmetrics.Available, Value: hostmetrics.Memory{TotalBytes: 1024, AvailableBytes: 512, Estimate: "estimate"}, Sample: "ram", AgeMillis: 20000}},
	}
	for i := range 30 {
		state.Sessions = append(state.Sessions, protocol.SessionInfo{ID: fmt.Sprintf("%04d", i), Label: "named shell", State: "running", Command: []string{strings.Repeat("x", 4096)}})
		state.Services = append(state.Services, protocol.ServiceInfo{Name: fmt.Sprintf("service-%02d", i), Healthy: true})
	}
	state.Services[29].Healthy = false
	state.Services[29].Problem = "reported failure"
	view := projectDashboardState(DashboardHost{ID: "host", Alias: "pc"}, state)
	if view.Sessions.Total != 30 || len(view.Sessions.Rows) != dashboardSessionLimit || view.Services.Total != 30 || len(view.Services.Rows) != dashboardServiceLimit {
		t.Fatalf("unbounded or false totals: %+v", view)
	}
	if view.Services.Ready != 29 || view.Services.Failed != 1 {
		t.Fatalf("full-catalog state counts lost beyond row bound: %+v", view.Services)
	}
	if view.Sessions.Rows[0].Name != "named shell" {
		t.Fatal("actual catalog label lost")
	}
	if view.Services.Rows[0].Name != "service-29" {
		t.Fatal("reported failure hidden by bounds")
	}
	if len(view.Sessions.Rows[0].Command) > dashboardTextLimit {
		t.Fatal("unbounded command")
	}
	if !view.CPU.MeasuredAt.Equal(now.Add(-100*time.Millisecond)) || !view.RAM.MeasuredAt.Equal(now.Add(-20*time.Second)) {
		t.Fatal("independent metric ages lost")
	}
	state.Sessions[0].Command[0] = "mutated"
	if view.Sessions.Rows[0].Command == "mutated" {
		t.Fatal("published rows share mutable input")
	}
}

func TestDashboardActualCLIControlNeverWakes(t *testing.T) {
	for _, mode := range []string{"watch", "old-producer", "legacy", "generic-error", "wrong-identity", "partial", "disconnected"} {
		t.Run(mode, func(t *testing.T) {
			fixture := setupCommandTestHost(t)
			stateDir, err := paths.StateDir()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := identity.LoadOrCreate(stateDir); err != nil {
				t.Fatal(err)
			}
			var recovery, wakes, connections, probes, lists, metrics atomic.Int32
			serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				_ = transport.Serve(w, r, func(ctx context.Context, conn transport.Conn) error {
					for ctx.Err() == nil {
						frame, err := conn.ReadFrame()
						if err != nil {
							return fmt.Errorf("fixture read: %w", err)
						}
						request, err := protocol.DecodeControl(frame.Payload)
						if err != nil {
							return fmt.Errorf("fixture control: %w", err)
						}
						response := protocol.Control{RequestID: request.RequestID}
						switch request.Type {
						case protocol.TypeHostInfo:
							id := fixture.host.ID
							if mode == "wrong-identity" {
								id = "another-host"
							}
							response.Type = protocol.TypeHostInfoResult
							response.Host = &protocol.HostInfo{ID: id, MeshIdentity: fixture.host.MeshIdentity, ServiceHealthSupported: mode != "legacy" && mode != "old-producer"}
						case protocol.TypeStateWatch:
							probes.Add(1)
							response.Type = protocol.TypeStateSnapshot
							response.StateSnapshot = &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "proxy", Healthy: true}}, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: fixture.host.ID, Command: []string{"shell"}, State: "running", CreatedAt: commandTestTime}}, Current: map[string]protocol.Observation{protocol.TopicSessions: {}, protocol.TopicServices: {}}, Metrics: &hostmetrics.Snapshot{CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Available, Value: 25, Sample: "cpu"}, RAM: hostmetrics.Reading[hostmetrics.Memory]{Availability: hostmetrics.Available, Value: hostmetrics.Memory{TotalBytes: 1024, AvailableBytes: 512, Estimate: "Linux MemAvailable estimate"}, Sample: "ram"}, Temperature: hostmetrics.Reading[hostmetrics.Temperature]{Availability: hostmetrics.Unsupported}, Uptime: hostmetrics.Reading[uint64]{Availability: hostmetrics.Available, Sample: "uptime"}}}
							if mode == "legacy" {
								response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: `daemon: unknown control "state.watch"`}
							}
							if mode == "generic-error" {
								response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: "permission denied"}
							}
							if mode == "partial" {
								response.StateSnapshot.Current[protocol.TopicServices] = protocol.Observation{Failing: true}
							}
						case protocol.TypeList:
							lists.Add(1)
							if !request.Lean {
								return errors.New("fallback omitted lean request")
							}
							response.Type = protocol.TypeListed
							response.Sessions = []protocol.SessionInfo{{ID: "7K3D", HostID: fixture.host.ID, State: "running", Command: []string{"shell"}, CreatedAt: commandTestTime}}
						case protocol.TypeServiceList:
							response.Type = protocol.TypeServiceListed
							response.Services = []protocol.ServiceInfo{{Name: "proxy", Healthy: true}}
						case protocol.TypeHostMetrics:
							metrics.Add(1)
							response.Type = protocol.TypeError
							response.Message = `daemon: unknown control "host.metrics"`
						default:
							return fmt.Errorf("unexpected dashboard control %q", request.Type)
						}
						if err := conn.WriteFrame(mustCommandControlFrame(response)); err != nil {
							return fmt.Errorf("fixture reply: %w", err)
						}
						if mode == "disconnected" && request.Type == protocol.TypeStateWatch {
							return nil
						}
					}
					return nil
				})
			}))
			defer serve.Close()
			fixture.host.Endpoint = "ws" + strings.TrimPrefix(serve.URL, "http") + "/control/ws"
			if err := SaveHost(fixture.host); err != nil {
				t.Fatal(err)
			}
			_, _, err = executeCommand(t, Dependencies{
				DialControl: dialControlHost,
				DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
					recovery.Add(1)
					return nil, errors.New("recovery reached")
				},
				Wake: func(context.Context, HostRecord) error { wakes.Add(1); return nil },
				Dashboard: func(ctx context.Context, input DashboardInput) error {
					if !input.Wall {
						t.Error("wall option lost")
					}
					run, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					reached := false
					err := input.Watch(run, func(view DashboardHostView) {
						if view.Host.ID != fixture.host.ID {
							return
						}
						done := view.Sessions.Total == 1
						switch mode {
						case "wrong-identity":
							done = view.Connection == StateRefused
						case "generic-error":
							done = view.Problem != "" && view.Connection == StateReachable
						case "legacy":
							done = view.CPU.State == hostmetrics.Unsupported && view.Sessions.Total == 1
						case "partial":
							done = done && view.Services.Failing
						case "disconnected":
							done = done && view.Connection == StateUnreachable
						}
						if done {
							if mode == "watch" && view.Services.Ready != 1 {
								t.Errorf("verified producer health advertisement lost: %+v", view.Services)
							}
							if (mode == "old-producer" || mode == "legacy") && (view.Services.Ready != 0 || view.Services.Failed != 0 || view.Services.Unknown != 1) {
								t.Errorf("missing advertisement manufactured health facts: %+v", view.Services)
							}
							reached = true
							cancel()
						}
					})
					if !reached {
						t.Errorf("dashboard never published expected %s state: %v", mode, err)
					}
					return err
				},
			}, "dashboard", "--wall")
			if err != nil {
				t.Fatal(err)
			}
			if recovery.Load() != 0 || wakes.Load() != 0 {
				t.Fatal("dashboard woke host", recovery.Load(), wakes.Load())
			}
			if mode == "watch" || mode == "old-producer" || mode == "partial" || mode == "disconnected" {
				if connections.Load() != 1 || probes.Load() != 1 || lists.Load() != 0 {
					t.Fatal("watch reopened or polled", connections.Load(), probes.Load(), lists.Load())
				}
			}
			if mode == "generic-error" && (lists.Load() != 0 || metrics.Load() != 0 || probes.Load() != 1) {
				t.Fatal("generic error enabled fallback")
			}
			if mode == "wrong-identity" && probes.Load() != 0 {
				t.Fatal("unverified host was queried")
			}
		})
	}
}

func TestDashboardTextIsOwnedBoundedUTF8AndTerminalSafe(t *testing.T) {
	for _, input := range []string{strings.Repeat("界", 200), strings.Repeat("x", 255) + "界", "malicious\x1b[2J\nrow\tname\u202e", string([]byte{0xff, 0xfe})} {
		for _, text := range []string{dashboardText(input), dashboardCommandText([]string{"sh", input})} {
			if len(text) > dashboardTextLimit || !utf8.ValidString(text) || strings.ContainsAny(text, "\x1b\n\r\t") || strings.ContainsRune(text, '\u202e') {
				t.Fatalf("unsafe bounded text: %q (%d bytes)", text, len(text))
			}
		}
	}
}

func TestDashboardCatalogNameSanitizedBoundedAndRetained(t *testing.T) {
	rows := []protocol.SessionInfo{{ID: "7K3D", Label: "\x1b[2J" + strings.Repeat("名前", 200), State: "running"}}
	catalog := projectDashboardSessions(rows, ObservedSection{})
	name := catalog.Rows[0].Name
	if len(name) > dashboardTextLimit || !utf8.ValidString(name) || strings.Contains(name, "\x1b") {
		t.Fatalf("unsafe/unbounded catalog name: %q", name)
	}
	rows[0].Label = "changed"
	if catalog.Rows[0].Name != name {
		t.Fatal("published label changed with source catalog")
	}
	empty := projectDashboardSessions([]protocol.SessionInfo{{ID: "empty", State: "running"}}, ObservedSection{})
	if empty.Rows[0].Name != "" {
		t.Fatal("missing real catalog label was invented")
	}
}
