package protocol

import (
	"fmt"
	"github.com/shaul/mesh/internal/hostmetrics"
	"time"
)

const (
	TopicHost               = "host"
	TopicSessions           = "sessions"
	TopicServices           = "services"
	TopicMetrics            = "metrics"
	TypeStateWatch          = "state.watch"
	TypeStateSnapshot       = "state.snapshot"
	TypeStateEvent          = "state.event"
	TypeStateResync         = "state.resync"
	TypeStateCurrent        = "state.current"
	TypeHostMetrics         = "host.metrics"
	TypeHostMetricsResult   = "host.metrics.result"
	ErrorCodeUnknownControl = "control.unknown"
	ErrorCodeWatchLimit     = "state.subscriber_limit"
)

type StateWatch struct {
	Topics             []string `json:"topics"`
	MetricsEveryMillis int64    `json:"metricsEvery,omitempty"`
}

func (w StateWatch) Validate() error {
	if len(w.Topics) == 0 || len(w.Topics) > 4 {
		return fmt.Errorf("state.watch requires one to four topics")
	}
	seen := map[string]bool{}
	for _, topic := range w.Topics {
		if topic != TopicHost && topic != "sessions" && topic != "services" && topic != "metrics" {
			return fmt.Errorf("state.watch unknown topic %q", topic)
		}
		if seen[topic] {
			return fmt.Errorf("state.watch duplicate topic %q", topic)
		}
		seen[topic] = true
	}
	if w.MetricsEveryMillis != 0 && (w.MetricsEveryMillis < hostmetrics.MinimumInterval.Milliseconds() || w.MetricsEveryMillis > hostmetrics.MaximumInterval.Milliseconds()) {
		return fmt.Errorf("state.watch metricsEvery must be 2000 to 10000 milliseconds")
	}
	return nil
}
func (w StateWatch) MetricsEvery() time.Duration {
	if w.MetricsEveryMillis == 0 {
		return hostmetrics.MinimumInterval
	}
	return time.Duration(w.MetricsEveryMillis) * time.Millisecond
}

type Observation struct {
	AgeMillis int64 `json:"ageMillis"`
	Failing   bool  `json:"failing,omitempty"`
}
type StateSnapshot struct {
	Host     *HostInfo                `json:"host,omitempty"`
	Memory   map[string]SessionMemory `json:"memory,omitempty"`
	Seq      uint64                   `json:"seq"`
	Sessions []SessionInfo            `json:"sessions,omitempty"`
	Services []ServiceInfo            `json:"services,omitempty"`
	Metrics  *hostmetrics.Snapshot    `json:"metrics,omitempty"`
	Current  map[string]Observation   `json:"current"`
}
type StateEvent struct {
	Seq     uint64       `json:"seq"`
	Kind    string       `json:"kind"`
	Payload StatePayload `json:"payload"`
}
type SessionMemory struct {
	Bytes     uint64 `json:"bytes"`
	AgeMillis int64  `json:"ageMillis"`
	Available bool   `json:"available"`
}
type StatePayload struct {
	Host        *HostInfo                `json:"host,omitempty"`
	Memory      map[string]SessionMemory `json:"memory,omitempty"`
	Session     *SessionInfo             `json:"session,omitempty"`
	SessionID   string                   `json:"sessionId,omitempty"`
	Service     *ServiceInfo             `json:"service,omitempty"`
	ServiceName string                   `json:"serviceName,omitempty"`
	Metrics     *hostmetrics.Snapshot    `json:"metrics,omitempty"`
}
type StateCurrent struct {
	Seq      uint64                 `json:"seq"`
	Sections map[string]Observation `json:"sections"`
}
