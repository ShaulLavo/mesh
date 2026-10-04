package cli

import (
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
)

func validateStateMessage(message protocol.Control) error {
	if err := validateStateName(message); err != nil {
		return err
	}
	if message.StateSnapshot != nil {
		snapshot := message.StateSnapshot
		if err := validateStateSections(snapshot.Current); err != nil {
			return err
		}
		if err := validateStateSamples(snapshot.Memory, snapshot.Metrics); err != nil {
			return err
		}
	}
	if message.StateEvent != nil {
		payload := message.StateEvent.Payload
		if err := validateStateSamples(payload.Memory, payload.Metrics); err != nil {
			return err
		}
	}
	if message.StateCurrent == nil {
		return nil
	}
	if message.StateEvent != nil && message.StateCurrent.Seq != message.StateEvent.Seq {
		return fmt.Errorf("state event and observation sequences differ")
	}
	return validateStateSections(message.StateCurrent.Sections)
}

func validateStateSections(sections map[string]protocol.Observation) error {
	for topic, observation := range sections {
		if topic != protocol.TopicSessions && topic != protocol.TopicServices && topic != protocol.TopicMetrics && topic != protocol.TopicHost {
			return fmt.Errorf("unknown state observation topic %q", topic)
		}
		if err := validateStateAge(observation.AgeMillis); err != nil {
			return err
		}
	}
	return nil
}
func validateStateMemory(memory map[string]protocol.SessionMemory) error {
	for _, reading := range memory {
		if err := validateStateAge(reading.AgeMillis); err != nil {
			return err
		}
	}
	return nil
}
func validateStateAge(age int64) error {
	if age < 0 || age > math.MaxInt64/int64(time.Millisecond) {
		return fmt.Errorf("invalid state observation age")
	}
	return nil
}
func validateMetricReading[T any](reading hostmetrics.Reading[T]) error {
	if err := validateStateAge(reading.AgeMillis); err != nil {
		return err
	}
	switch reading.Availability {
	case hostmetrics.Available, hostmetrics.Unavailable, hostmetrics.Unsupported:
		return nil
	default:
		return fmt.Errorf("invalid state metric availability")
	}
}
func validateStateMetrics(metrics *hostmetrics.Snapshot) error {
	if metrics == nil {
		return nil
	}
	if err := validatePerformance(metrics); err != nil {
		return err
	}
	if err := validateMetricReading(metrics.CPU); err != nil {
		return err
	}
	if err := validateMetricReading(metrics.RAM); err != nil {
		return err
	}
	if err := validateMetricReading(metrics.Temperature); err != nil {
		return err
	}
	if err := validateMetricReading(metrics.Uptime); err != nil {
		return err
	}
	if math.IsNaN(metrics.CPU.Value) || math.IsInf(metrics.CPU.Value, 0) || metrics.CPU.Value < 0 || metrics.CPU.Value > 100 {
		return fmt.Errorf("invalid state CPU utilization")
	}
	if metrics.RAM.Availability == hostmetrics.Available && (metrics.RAM.Value.TotalBytes == 0 || metrics.RAM.Value.AvailableBytes > metrics.RAM.Value.TotalBytes || metrics.RAM.Value.Estimate == "") {
		return fmt.Errorf("invalid state RAM estimate")
	}
	temperature := metrics.Temperature.Value.Celsius
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < -273.15 || temperature > 1000 {
		return fmt.Errorf("invalid state CPU temperature")
	}
	if metrics.Temperature.Availability == hostmetrics.Available && metrics.Temperature.Value.Sensor == "" {
		return fmt.Errorf("state CPU temperature requires a sensor")
	}
	return nil
}

func validateStateName(message protocol.Control) error {
	if err := validateNameEnvelope(message); err != nil {
		return err
	}
	if info := stateDeclaredHost(message); info != nil {
		if err := machinename.ValidateClaim(info.ID, declaredName(*info)); err != nil {
			return fmt.Errorf("invalid state host name: %w", err)
		}
	}
	return nil
}

func validateNameEnvelope(message protocol.Control) error {
	remainder := message
	remainder.Type, remainder.RequestID = "", ""
	switch message.Type {
	case protocol.TypeStateSnapshot:
		if message.StateSnapshot == nil {
			return fmt.Errorf("missing state snapshot")
		}
		remainder.StateSnapshot = nil
	case protocol.TypeStateEvent:
		if message.StateEvent == nil {
			return fmt.Errorf("missing state event")
		}
		if err := validateEventPayload(*message.StateEvent); err != nil {
			return err
		}
		if message.StateCurrent != nil && message.StateCurrent.Seq != message.StateEvent.Seq {
			return fmt.Errorf("state event and observation sequences differ")
		}
		remainder.StateEvent, remainder.StateCurrent = nil, nil
	case protocol.TypeStateCurrent:
		if message.StateCurrent == nil {
			return fmt.Errorf("missing state observation")
		}
		remainder.StateCurrent = nil
	case protocol.TypeHostInfoResult:
		if message.Host == nil {
			return fmt.Errorf("missing host info")
		}
		remainder.Host = nil
	default:
		return nil
	}
	if !reflect.ValueOf(remainder).IsZero() {
		return fmt.Errorf("unexpected members in %s envelope", message.Type)
	}
	return nil
}

func validateEventPayload(event protocol.StateEvent) error {
	remainder := event.Payload
	switch event.Kind {
	case "host.changed":
		if remainder.Host == nil {
			return fmt.Errorf("missing host event payload")
		}
		remainder.Host = nil
	case "session.added", "session.changed":
		if remainder.Session == nil {
			return fmt.Errorf("missing session event payload")
		}
		remainder.Session = nil
	case "session.memory":
		remainder.Memory = nil
	case "session.removed":
		remainder.SessionID = ""
	case "service.changed":
		if remainder.Service == nil {
			return fmt.Errorf("missing service event payload")
		}
		remainder.Service = nil
	case "service.removed":
		remainder.ServiceName = ""
	case protocol.TopicMetrics:
		if remainder.Metrics == nil {
			return fmt.Errorf("missing metrics event payload")
		}
		remainder.Metrics = nil
	default:
		return fmt.Errorf("unknown state event %q", event.Kind)
	}
	if !reflect.ValueOf(remainder).IsZero() {
		return fmt.Errorf("unexpected members in %s event payload", event.Kind)
	}
	return nil
}

func validateStateSamples(memory map[string]protocol.SessionMemory, metrics *hostmetrics.Snapshot) error {
	if err := validateStateMemory(memory); err != nil {
		return err
	}
	return validateStateMetrics(metrics)
}
