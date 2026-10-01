package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestWatchPickerActualControlTransportNeverWakes(t *testing.T) {
	for _, mode := range []string{"watch", "legacy", "generic-error", "wrong-identity"} {
		t.Run(mode, func(t *testing.T) {
			fixture := setupCommandTestHost(t)
			var recoveryDials, wakes, lists, inspections, probes atomic.Int32
			rejected := make(chan struct{}, 4)
			serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = transport.Serve(w, r, func(ctx context.Context, conn transport.Conn) error {
					for ctx.Err() == nil {
						frame, err := conn.ReadFrame()
						if err != nil {
							return fmt.Errorf("fixture control read: %w", err)
						}
						request, err := protocol.DecodeControl(frame.Payload)
						if err != nil {
							return fmt.Errorf("fixture control decode: %w", err)
						}
						response := protocol.Control{RequestID: request.RequestID}
						rows := []protocol.SessionInfo{{ID: "7K3D", HostID: fixture.host.ID, Command: []string{"shell"}, State: "running", CreatedAt: commandTestTime}}
						switch request.Type {
						case protocol.TypeHostInfo:
							id := fixture.host.ID
							if mode == "wrong-identity" {
								id = "other-host"
							}
							response.Type = protocol.TypeHostInfoResult
							response.Host = &protocol.HostInfo{ID: id, MeshIdentity: fixture.host.MeshIdentity}
						case protocol.TypeStateWatch:
							probes.Add(1)
							response.Type = protocol.TypeStateSnapshot
							response.StateSnapshot = &protocol.StateSnapshot{Seq: 1, Sessions: rows, Memory: map[string]protocol.SessionMemory{"7K3D": {Bytes: 64, Available: true, AgeMillis: math.MaxInt64 / int64(time.Millisecond)}}, Current: map[string]protocol.Observation{protocol.TopicSessions: {}}}
							if mode == "legacy" {
								response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: `daemon: unknown control "state.watch"`}
							}
							if mode == "generic-error" {
								response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: "permission denied"}
							}
						case protocol.TypeList:
							lists.Add(1)
							if !request.Lean {
								return errors.New("catalog request omitted lean")
							}
							response.Type = protocol.TypeListed
							response.Sessions = rows
						case protocol.TypeServiceList:
							response.Type = protocol.TypeServiceListed
						case protocol.TypeInspect:
							inspections.Add(1)
							response.Type = protocol.TypeInspected
							response.SessionID = request.SessionID
							response.Inspection = &protocol.SessionInspection{ObservedAt: commandTestTime, Preview: []string{"explicit preview"}}
						default:
							return errors.New("unexpected fixture control")
						}
						if err := conn.WriteFrame(mustCommandControlFrame(response)); err != nil {
							return fmt.Errorf("fixture control write: %w", err)
						}
						if mode == "wrong-identity" || mode == "generic-error" && request.Type == protocol.TypeStateWatch {
							select {
							case rejected <- struct{}{}:
							default:
							}
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
			_, _, err := executeCommand(t, Dependencies{
				DialControl: dialControlHost,
				DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
					recoveryDials.Add(1)
					return nil, errors.New("recovery path reached")
				},
				Wake: func(context.Context, HostRecord) error { wakes.Add(1); return nil },
				Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
					refresh, cancel := context.WithCancel(ctx)
					defer cancel()
					if mode == "generic-error" || mode == "wrong-identity" {
						go func() {
							select {
							case <-rejected:
								cancel()
							case <-refresh.Done():
							}
						}()
					}
					result, refreshErr := input.Refresh(refresh, fixture.host.Alias)
					if mode == "generic-error" || mode == "wrong-identity" {
						if refreshErr == nil {
							t.Error("refused host published usable rows")
						}
						return PickerSelection{}, nil
					}
					if refreshErr != nil || len(result.Sessions.Sessions) != 1 {
						t.Fatalf("refresh = %+v, %v", result, refreshErr)
					}
					if result.Sessions.Sessions[0].MemoryBytes != 0 {
						t.Fatal("expired session memory became fresh through duration overflow")
					}
					inspection, inspectErr := input.Inspect(refresh, PickerInspectRequest{HostAlias: fixture.host.Alias, SessionID: "7K3D", PreviewCols: 80, PreviewRows: 24})
					if inspectErr != nil || len(inspection.Preview) != 1 || inspection.Preview[0] != "explicit preview" {
						t.Fatalf("explicit preview = %+v, %v", inspection, inspectErr)
					}
					return PickerSelection{}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if recoveryDials.Load() != 0 || wakes.Load() != 0 {
				t.Fatal("read-only picker reached wake/recovery", recoveryDials.Load(), wakes.Load())
			}
			// Picker startup paints its persisted catalog; only legacy watch fallback lists.
			if mode == "generic-error" && (lists.Load() != 0 || probes.Load() != 1) {
				t.Fatal("generic error enabled polling", lists.Load())
			}
			if mode == "legacy" && (lists.Load() != 1 || probes.Load() != 1) {
				t.Fatal("explicit unknown did not enable polling", lists.Load())
			}
			if (mode == "watch" || mode == "legacy") && inspections.Load() != 1 {
				t.Fatal("preview inspection was lost")
			}
		})
	}
}
