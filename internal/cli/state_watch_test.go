package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

func TestWatchViewGapsFailureSilenceAndOwnership(t *testing.T) {
	now := time.Now()
	view := StateView{}
	snapshot := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Current: map[string]protocol.Observation{"sessions": {}}, Sessions: []protocol.SessionInfo{{ID: "7K3D", Command: []string{"shell"}}}}}
	if err := view.Apply(snapshot, now, 0); err != nil {
		t.Fatal(err)
	}
	snapshot.StateSnapshot.Sessions[0].Command[0] = "mutation"
	cloned := view.Clone()
	cloned.Sessions[0].Command[0] = "copy mutation"
	if view.Sessions[0].Command[0] != "shell" {
		t.Fatal("aliasing")
	}
	for i := uint64(2); i <= 32; i++ {
		now = now.Add(10 * time.Second)
		if err := view.Apply(protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: &protocol.StateCurrent{Seq: i, Sections: map[string]protocol.Observation{"sessions": {}}}}, now, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if view.Sections["sessions"].Stale(now, view.LastReply) {
		t.Fatal("unchanged catalog stale after five minutes")
	}
	if err := view.Apply(protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: &protocol.StateCurrent{Seq: 33, Sections: map[string]protocol.Observation{"sessions": {Failing: true}}}}, now, 0); err != nil {
		t.Fatal(err)
	}
	if !view.Sections["sessions"].Stale(now, view.LastReply) {
		t.Fatal("failure hidden by current")
	}
	if err := view.Apply(protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: &protocol.StateCurrent{Seq: 35}}, now, 0); !errors.Is(err, ErrStateGap) {
		t.Fatal(err)
	}
	if !view.Sections["sessions"].Stale(now.Add(30*time.Second), view.LastReply) {
		t.Fatal("silence fresh")
	}
}
func TestWatchSnapshotKeepsSameMetricAging(t *testing.T) {
	now := time.Now()
	view := StateView{}
	snapshot := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Metrics: &hostmetrics.Snapshot{CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Available, Sample: "same"}, RAM: hostmetrics.Reading[hostmetrics.Memory]{Availability: hostmetrics.Unavailable}, Temperature: hostmetrics.Reading[hostmetrics.Temperature]{Availability: hostmetrics.Unsupported}, Uptime: hostmetrics.Reading[uint64]{Availability: hostmetrics.Unavailable}}, Current: map[string]protocol.Observation{}}}
	if err := view.Apply(snapshot, now, 0); err != nil {
		t.Fatal(err)
	}
	snapshot.StateSnapshot.Seq = 2
	if err := view.Apply(snapshot, now.Add(6*time.Second), 0); err != nil {
		t.Fatal(err)
	}
	if view.Metrics.CPU.AgeMillis < 6000 {
		t.Fatal("same metric rejuvenated", view.Metrics.CPU.AgeMillis)
	}
}
func TestWatchExplicitUnknownOnly(t *testing.T) {
	for _, response := range []protocol.Control{{Type: protocol.TypeError, Message: "timeout"}, {Type: protocol.TypeError, Message: "unknown control"}, {Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeUnknownControl, Message: "permission denied"}, {Type: protocol.TypeError, Message: `daemon: unknown control "other"`}} {
		if explicitUnknownControl(response, protocol.TypeStateWatch) {
			t.Fatal(response)
		}
	}
	if !explicitUnknownControl(protocol.Control{Type: protocol.TypeError, Message: `daemon: unknown control "state.watch"`}, protocol.TypeStateWatch) {
		t.Fatal("legacy unknown refused")
	}
}
func TestWatchVerifiedControlBuildReprobeAndCancellation(t *testing.T) {
	host := HostRecord{MachineName: "host", ID: "host", MeshIdentity: "identity"}
	var probes atomic.Int32
	var polls atomic.Int32
	var version atomic.Int32
	var wrong atomic.Bool
	dial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		client, server := net.Pipe()
		conn, err := transport.NewStreamConn(client)
		if err != nil {
			return nil, fmt.Errorf("watch test transport: %w", err)
		}
		peer, err := transport.NewStreamConn(server)
		if err != nil {
			return nil, fmt.Errorf("watch test transport: %w", err)
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
				response := protocol.Control{RequestID: request.RequestID}
				switch request.Type {
				case protocol.TypeHostInfo:
					response.Type = protocol.TypeHostInfoResult
					id := host.ID
					if wrong.Load() {
						id = "wrong"
					}
					response.Host = &protocol.HostInfo{ID: id, MeshIdentity: host.MeshIdentity, Build: &release.Build{Version: time.Unix(int64(version.Load()), 0).String()}}
				case protocol.TypeStateWatch:
					probes.Add(1)
					response.Type = protocol.TypeError
					response.Message = `daemon: unknown control "state.watch"`
				case protocol.TypeList:
					polls.Add(1)
					response.Type = protocol.TypeListed
				default:
					return
				}
				payload, err := response.Encode()
				if err != nil {
					return
				}
				if err := peer.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
					return
				}
			}
		}()
		return conn, nil
	}
	watcher := NewStateWatcher(dial)
	request := protocol.StateWatch{Topics: []string{"sessions"}}
	run := func() {
		ctx, cancel := context.WithCancel(t.Context())
		view := StateView{Sections: map[string]ObservedSection{}}
		err := watcher.watchOnce(ctx, host, request, &view, func(StateView) { cancel() })
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	run()
	run()
	if probes.Load() != 1 || polls.Load() != 2 {
		t.Fatal(probes.Load(), polls.Load())
	}
	version.Add(1)
	run()
	if probes.Load() != 2 {
		t.Fatal("build was not reprobed")
	}
	wrong.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := watcher.watchOnce(ctx, host, request, &StateView{}, func(StateView) { t.Fatal("wrong identity published") })
	if err == nil {
		t.Fatal("wrong identity accepted")
	}
	if probes.Load() != 2 {
		t.Fatal("wrong identity probed")
	}
}
