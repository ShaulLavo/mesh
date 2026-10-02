package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"maps"
	"slices"
	"strings"
	"time"
)

var ErrStateGap = errors.New("state watch sequence gap")

type ObservedSection struct {
	Observation protocol.Observation
	ReceivedAt  time.Time
}

func (s ObservedSection) Stale(now, lastReply time.Time) bool {
	reading := hostmetrics.Reading[struct{}]{Availability: hostmetrics.Available, AgeMillis: s.Observation.AgeMillis, Failing: s.Observation.Failing}
	return MetricStale(reading, s.ReceivedAt, lastReply, now, 30*time.Second)
}

type StateConnection string

const (
	StateConnecting  StateConnection = "connecting"
	StateReachable   StateConnection = "reachable"
	StateUnreachable StateConnection = "unreachable"
	StateRefused     StateConnection = "refused"
)

type StateView struct {
	Connection        StateConnection
	Problem           string
	Memory            map[string]protocol.SessionMemory
	MemoryReceivedAt  time.Time
	Seq               uint64
	Sessions          []protocol.SessionInfo
	Services          []protocol.ServiceInfo
	Metrics           *hostmetrics.Snapshot
	Sections          map[string]ObservedSection
	LastReply         time.Time
	MetricsReceivedAt time.Time
	initialized       bool
}

func (v StateView) Clone() StateView {
	cloned := v
	cloned.Memory = maps.Clone(v.Memory)
	cloned.Sessions = cloneSessionInfo(v.Sessions)
	cloned.Services = cloneWireServices(v.Services)
	cloned.Sections = maps.Clone(v.Sections)
	if v.Metrics != nil {
		metrics := hostmetrics.AgeSnapshot(*v.Metrics, 0)
		cloned.Metrics = &metrics
	}
	return cloned
}

// Apply ages from receipt using the verified setup round trip for stream transit.
func (v *StateView) Apply(message protocol.Control, received time.Time, transit time.Duration) error {
	if err := validateStateMessage(message); err != nil {
		return err
	}
	if message.Type == protocol.TypeStateResync {
		v.initialized = false
		return nil
	}
	if message.Type == protocol.TypeStateSnapshot {
		return v.applySnapshot(message.StateSnapshot, received, transit)
	}
	if !v.initialized {
		return ErrStateGap
	}
	seq := uint64(0)
	switch message.Type {
	case protocol.TypeStateEvent:
		if message.StateEvent != nil {
			seq = message.StateEvent.Seq
		}
	case protocol.TypeStateCurrent:
		if message.StateCurrent != nil {
			seq = message.StateCurrent.Seq
		}
	default:
		return fmt.Errorf("unexpected state watch control %q", message.Type)
	}
	if seq != v.Seq+1 {
		return ErrStateGap
	}
	if message.StateEvent != nil {
		if err := v.applyEvent(*message.StateEvent, received, transit); err != nil {
			return err
		}
	}
	if message.StateCurrent != nil {
		v.applyCurrent(message.StateCurrent.Sections, received, transit)
	}
	v.Seq = seq
	v.LastReply = received
	return nil
}
func (v *StateView) applyCurrent(sections map[string]protocol.Observation, received time.Time, transit time.Duration) {
	for name, observation := range sections {
		observation.AgeMillis += max(0, transit.Milliseconds())
		v.Sections[name] = ObservedSection{Observation: observation, ReceivedAt: received}
	}
}
func (v *StateView) applyMetrics(metrics *hostmetrics.Snapshot, received time.Time, transit time.Duration) {
	if metrics == nil {
		return
	}
	cloned := hostmetrics.AgeSnapshot(*metrics, max(0, transit.Milliseconds()))
	if v.Metrics != nil {
		elapsed := max(0, received.Sub(v.MetricsReceivedAt).Milliseconds())
		retainMetricAge(&cloned.CPU, v.Metrics.CPU, elapsed)
		retainMetricAge(&cloned.RAM, v.Metrics.RAM, elapsed)
		retainMetricAge(&cloned.Temperature, v.Metrics.Temperature, elapsed)
		retainMetricAge(&cloned.Uptime, v.Metrics.Uptime, elapsed)
		retainPerformanceAge(&cloned, *v.Metrics, elapsed)
	}
	v.Metrics = &cloned
	v.MetricsReceivedAt = received
}
func retainMetricAge[T any](next *hostmetrics.Reading[T], old hostmetrics.Reading[T], elapsed int64) {
	if next.Sample != "" && next.Sample == old.Sample {
		next.AgeMillis = max(next.AgeMillis, old.AgeMillis+elapsed)
	}
}
func MetricStale[T any](metric hostmetrics.Reading[T], received, lastReply, now time.Time, limit time.Duration) bool {
	return metric.AgeMillis < 0 || metric.AgeMillis >= limit.Milliseconds() || metric.Failing || metric.Availability != hostmetrics.Available || received.IsZero() || lastReply.IsZero() || now.Sub(lastReply) >= 30*time.Second || time.Duration(metric.AgeMillis)*time.Millisecond+now.Sub(received) >= limit
}
func (v *StateView) applyEvent(event protocol.StateEvent, received time.Time, transit time.Duration) error {
	p := event.Payload
	switch event.Kind {
	case "session.added", "session.changed":
		if p.Session == nil {
			return errors.New("missing session event payload")
		}
		rows := cloneSessionInfo([]protocol.SessionInfo{*p.Session})
		v.Sessions = slices.DeleteFunc(v.Sessions, func(row protocol.SessionInfo) bool { return row.ID == p.Session.ID })
		v.Sessions = append(v.Sessions, rows[0])
		slices.SortFunc(v.Sessions, func(a, b protocol.SessionInfo) int { return strings.Compare(a.ID, b.ID) })
	case "session.memory":
		v.applyMemory(p.Memory, received, transit)
	case "session.removed":
		delete(v.Memory, p.SessionID)
		v.Sessions = slices.DeleteFunc(v.Sessions, func(row protocol.SessionInfo) bool { return row.ID == p.SessionID })
	case "service.changed":
		if p.Service == nil {
			return errors.New("missing service event payload")
		}
		rows := cloneWireServices([]protocol.ServiceInfo{*p.Service})
		v.Services = slices.DeleteFunc(v.Services, func(row protocol.ServiceInfo) bool { return row.Name == p.Service.Name })
		v.Services = append(v.Services, rows[0])
		slices.SortFunc(v.Services, func(a, b protocol.ServiceInfo) int { return strings.Compare(a.Name, b.Name) })
	case "service.removed":
		v.Services = slices.DeleteFunc(v.Services, func(row protocol.ServiceInfo) bool { return row.Name == p.ServiceName })
	case protocol.TopicMetrics:
		if p.Metrics == nil {
			return errors.New("missing metrics event payload")
		}
		v.applyMetrics(p.Metrics, received, transit)
	default:
		return fmt.Errorf("unknown state event %q", event.Kind)
	}
	return nil
}
func cloneWireServices(rows []protocol.ServiceInfo) []protocol.ServiceInfo {
	data, _ := json.Marshal(rows)
	var cloned []protocol.ServiceInfo
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func (v *StateView) applyMemory(memory map[string]protocol.SessionMemory, received time.Time, transit time.Duration) {
	if memory == nil {
		return
	}
	v.Memory = maps.Clone(memory)
	for id, reading := range v.Memory {
		reading.AgeMillis += max(0, transit.Milliseconds())
		v.Memory[id] = reading
	}
	v.MemoryReceivedAt = received
}

func (v *StateView) applySnapshot(snapshot *protocol.StateSnapshot, received time.Time, transit time.Duration) error {
	if snapshot == nil || snapshot.Seq == 0 {
		return errors.New("invalid state snapshot")
	}
	s := snapshot
	metrics, metricsReceived := v.Metrics, v.MetricsReceivedAt
	*v = StateView{Connection: v.Connection, Seq: s.Seq, Sessions: cloneSessionInfo(s.Sessions), Services: cloneWireServices(s.Services), Sections: map[string]ObservedSection{}, LastReply: received, initialized: true, Metrics: metrics, MetricsReceivedAt: metricsReceived}
	v.applyMemory(s.Memory, received, transit)
	v.applyCurrent(s.Current, received, transit)
	v.applyMetrics(s.Metrics, received, transit)
	return nil
}
