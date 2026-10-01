package cli

import (
	"math"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
)

func TestWatchRejectsMalformedAgeAndCurrentSequence(t *testing.T) {
	for _, age := range []int64{-1, math.MaxInt64} {
		view := StateView{}
		message := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Current: map[string]protocol.Observation{protocol.TopicSessions: {AgeMillis: age}}}}
		if err := view.Apply(message, time.Now(), 0); err == nil {
			t.Fatal("invalid age accepted", age)
		}
		if !view.LastReply.IsZero() {
			t.Fatal("invalid snapshot replaced retained state")
		}
	}
	view := StateView{}
	now := time.Now()
	if err := view.Apply(protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1}}, now, 0); err != nil {
		t.Fatal(err)
	}
	message := protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "session.removed", Payload: protocol.StatePayload{SessionID: "7K3D"}}, StateCurrent: &protocol.StateCurrent{Seq: 99}}
	if err := view.Apply(message, now, 0); err == nil {
		t.Fatal("event and current disagree on sequence")
	}
}

func TestWatchFreshMetricsDoNotAgeByStreamCadence(t *testing.T) {
	now := time.Now()
	metrics := hostmetrics.Snapshot{CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Available, Sample: "new"}, RAM: hostmetrics.Reading[hostmetrics.Memory]{Availability: hostmetrics.Unavailable}, Temperature: hostmetrics.Reading[hostmetrics.Temperature]{Availability: hostmetrics.Unsupported}, Uptime: hostmetrics.Reading[uint64]{Availability: hostmetrics.Unavailable}}
	view := StateView{}
	if err := view.Apply(protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1}}, now, 0); err != nil {
		t.Fatal(err)
	}
	if err := view.Apply(protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: protocol.TopicMetrics, Payload: protocol.StatePayload{Metrics: &metrics}}}, now.Add(10*time.Second), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if view.Metrics.CPU.AgeMillis != 5 || MetricStale(view.Metrics.CPU, view.MetricsReceivedAt, view.LastReply, now.Add(10*time.Second), 10*time.Second) {
		t.Fatal("idle cadence aged a fresh sample", view.Metrics.CPU)
	}
}
