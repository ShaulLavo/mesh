package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	view := projectDashboardState(DashboardHost{ID: "host", MachineName: "pc"}, state)
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
			auth, hostID := controlFixtureAuthentication(t)
			fixture.host.ID, fixture.host.MeshIdentity = hostID, hostID
			configPath, err := ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(configPath); err != nil {
				t.Fatal(err)
			}
			stateDir, err := paths.StateDir()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := identity.LoadOrCreate(stateDir); err != nil {
				t.Fatal(err)
			}
			var recovery, wakes, connections, probes, lists, metrics, inspections atomic.Int32
			serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
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
						case protocol.TypeInspect:
							inspections.Add(1)
							if request.PreviewCols != 1 || request.PreviewRows != 1 {
								return errors.New("dashboard requested full preview")
							}
							response.Type, response.SessionID = protocol.TypeInspected, request.SessionID
							response.Inspection = &protocol.SessionInspection{ObservedAt: time.Now(), ForegroundCommand: "claude", CurrentDirectory: "/work/mesh", DirectorySource: protocol.DirectorySourceProcess}
						case protocol.TypeHostInfo:
							id := fixture.host.ID
							if mode == "wrong-identity" {
								id = "another-host"
							}
							response.Type = protocol.TypeHostInfoResult
							response.Host = &protocol.HostInfo{ID: id, MeshIdentity: fixture.host.MeshIdentity, MachineName: fixture.host.MachineName, NameRevision: 1, ServiceHealthSupported: mode != "legacy" && mode != "old-producer"}
						case protocol.TypeStateWatch:
							probes.Add(1)
							response.Type = protocol.TypeStateSnapshot
							response.StateSnapshot = &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "proxy", Healthy: true}}, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: fixture.host.ID, Command: []string{"shell"}, State: "running", CreatedAt: commandTestTime}}, Current: map[string]protocol.Observation{protocol.TopicSessions: {}, protocol.TopicServices: {}}, Metrics: &hostmetrics.Snapshot{CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Available, Value: 25, Sample: "cpu"}, RAM: hostmetrics.Reading[hostmetrics.Memory]{Availability: hostmetrics.Available, Value: hostmetrics.Memory{TotalBytes: 1024, AvailableBytes: 512, Estimate: "Linux MemAvailable estimate"}, Sample: "ram"}, Temperature: hostmetrics.Reading[hostmetrics.Temperature]{Availability: hostmetrics.Unsupported}, Uptime: hostmetrics.Reading[uint64]{Availability: hostmetrics.Available, Sample: "uptime"}}}
							if mode != "old-producer" {
								response.StateSnapshot.Host = &protocol.HostInfo{ID: fixture.host.ID, MeshIdentity: fixture.host.MeshIdentity, MachineName: fixture.host.MachineName, NameRevision: 1}
								response.StateSnapshot.Current[protocol.TopicHost] = protocol.Observation{}
							}
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
			if err := saveNamedTestHost(t, fixture.host); err != nil {
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
							inspectDashboardFixture(run, t, mode, input, fixture.host)
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
			if mode == "watch" && inspections.Load() != 1 {
				t.Fatal("dashboard did not request exactly one live observation")
			}
			if mode == "watch" || mode == "old-producer" || mode == "partial" || mode == "disconnected" {
				wantConnections := 1 + inspections.Load()
				if connections.Load() != wantConnections || probes.Load() != 1 || lists.Load() != 0 {
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

func inspectDashboardFixture(ctx context.Context, t *testing.T, mode string, input DashboardInput, host HostRecord) {
	t.Helper()
	if mode != "watch" {
		return
	}
	inspection, err := input.Inspect(ctx, PickerInspectRequest{HostID: host.ID, SessionID: "7K3D"})
	if err != nil || inspection.ForegroundCommand != "claude" {
		t.Errorf("dashboard inspection failed: %+v / %v", inspection, err)
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
	if empty.Rows[0].Name != "Terminal" || empty.Rows[0].Label != "" {
		t.Fatal("unidentified terminal should have an honest fallback without inventing an explicit label")
	}
}

func TestDashboardUnnamedSessionsUseTheirStartingDirectory(t *testing.T) {
	rows := []protocol.SessionInfo{{ID: "7K3D", State: "detached", Cwd: "/work/projects/mesh", Command: []string{"sh", "-c", `cd -- "$1" && exec "${SHELL:-/bin/bash}" -l`}}}
	catalog := projectDashboardSessions(rows, ObservedSection{})
	if got := catalog.Rows[0].Name; got != "mesh" {
		t.Fatalf("unnamed session displayed as %q, want its project mesh", got)
	}
}
