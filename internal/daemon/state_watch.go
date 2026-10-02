package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
)

const DefaultSubscriberLimit = 64
const stateQueueCapacity = 64

var errWatchLimit = errors.New("daemon: state.watch subscriber limit reached")

type sectionObservation struct {
	at      time.Time
	failing bool
}
type stateSubscriber struct {
	confirm bool
	topics  map[string]bool
	pending map[string]protocol.StateEvent
	order   []string
	resync  bool
	wake    chan struct{}
	seq     uint64
}
type stateBroker struct {
	projection   chan struct{}
	dirty        map[storage.SessionID]bool
	revisions    map[storage.SessionID]uint64
	revision     uint64
	activity     chan struct{}
	stored       map[storage.SessionID]storage.Session
	memory       map[string]protocol.SessionMemory
	memoryAt     time.Time
	mu           sync.Mutex
	now          func() time.Time
	limit        int
	sessions     map[string]protocol.SessionInfo
	services     map[string]protocol.ServiceInfo
	observations map[string]sectionObservation
	subscribers  map[*stateSubscriber]struct{}
	metrics      *hostmetrics.Snapshot
	metricsAt    time.Time
}

func newStateBroker(limit int, now func() time.Time) *stateBroker {
	if limit == 0 {
		limit = DefaultSubscriberLimit
	}
	return &stateBroker{projection: make(chan struct{}, 1), dirty: map[storage.SessionID]bool{}, revisions: map[storage.SessionID]uint64{}, activity: make(chan struct{}, 1), now: now, limit: limit, stored: map[storage.SessionID]storage.Session{}, memory: map[string]protocol.SessionMemory{}, sessions: map[string]protocol.SessionInfo{}, services: map[string]protocol.ServiceInfo{}, observations: map[string]sectionObservation{}, subscribers: map[*stateSubscriber]struct{}{}}
}
func (b *stateBroker) observeSessions(err error) { b.observe(protocol.TopicSessions, err) }
func (b *stateBroker) observeServices(err error) { b.observe(protocol.TopicServices, err) }
func (b *stateBroker) observe(topic string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o := b.observations[topic]
	o.failing = err != nil
	if err == nil {
		o.at = b.now()
	}
	b.updateObservationLocked(topic, o)
}
func (b *stateBroker) sessionsChanged(diff SessionDiff) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updateObservationLocked(protocol.TopicSessions, sectionObservation{at: b.now()})
	for _, id := range diff.MetadataChanged {
		b.markRecognitionLocked(id)
	}
	for _, id := range diff.Removed {
		b.markRecognitionLocked(id)
	}
	for _, row := range diff.Added {
		b.markRecognitionLocked(row.ID)
		b.sessionLocked("session.added", row)
	}
	for _, row := range diff.Changed {
		b.sessionLocked("session.changed", row)
	}
	for _, id := range diff.Removed {
		delete(b.sessions, string(id))
		delete(b.stored, id)
		delete(b.dirty, id)
		delete(b.revisions, id)
		delete(b.memory, string(id))
		b.publishLocked(protocol.TopicSessions, "session/"+string(id), protocol.StateEvent{Kind: "session.removed", Payload: protocol.StatePayload{SessionID: string(id)}})
	}
}
func (b *stateBroker) sessionLocked(kind string, row storage.Session) {
	b.stored[row.ID] = cloneStoredSession(row)
	b.markProjectionLocked(row.ID)
	info := sessionInfo(row)
	if previous, exists := b.sessions[info.ID]; exists {
		retainRecognition(&info, previous)
	}
	info.RecoveryPending = true
	info.RecoveryDetailsOmitted = true
	b.sessions[info.ID] = info
	b.publishLocked(protocol.TopicSessions, "session/"+info.ID, protocol.StateEvent{Kind: kind, Payload: protocol.StatePayload{Session: &info}})
}

// servicesCommitted receives final routing state under the controller gate.
func (b *stateBroker) servicesCommitted(rows []protocol.ServiceInfo, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o := b.observations[protocol.TopicServices]
	o.failing = err != nil
	if err != nil {
		b.updateObservationLocked(protocol.TopicServices, o)
		return
	}
	o.at = b.now()
	b.updateObservationLocked(protocol.TopicServices, o)
	next := make(map[string]protocol.ServiceInfo, len(rows))
	for _, row := range rows {
		row = cloneState(row)
		next[row.Name] = row
		if old, exists := b.services[row.Name]; !exists || !reflect.DeepEqual(old, row) {
			b.publishLocked(protocol.TopicServices, "service/"+row.Name, protocol.StateEvent{Kind: "service.changed", Payload: protocol.StatePayload{Service: &row}})
		}
	}
	for name := range b.services {
		if _, exists := next[name]; !exists {
			b.publishLocked(protocol.TopicServices, "service/"+name, protocol.StateEvent{Kind: "service.removed", Payload: protocol.StatePayload{ServiceName: name}})
		}
	}
	b.services = next
}
func (b *stateBroker) serviceDemandChanged(next protocol.ServiceInfo, retiring bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	row, exists := b.services[next.Name]
	if retiring && next.Demand == nil {
		if exists {
			delete(b.services, next.Name)
			b.publishLocked(protocol.TopicServices, "service/"+next.Name, protocol.StateEvent{Kind: "service.removed", Payload: protocol.StatePayload{ServiceName: next.Name}})
		}
		return
	}
	if !retiring {
		if !exists || reflect.DeepEqual(row.Demand, next.Demand) {
			return
		}
		demand := next.Demand
		next = row
		next.Demand = demand
		next.Healthy, next.Problem, next.HealthUnknown = false, "", true
	}
	if !exists && len(b.services) >= meshserve.MaximumServices || exists && reflect.DeepEqual(row, next) {
		return
	}
	next = cloneState(next)
	b.services[next.Name] = next
	b.publishLocked(protocol.TopicServices, "service/"+next.Name, protocol.StateEvent{Kind: "service.changed", Payload: protocol.StatePayload{Service: &next}})
}
func (b *stateBroker) metricsChanged(metrics hostmetrics.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = &metrics
	b.metricsAt = b.now()
	b.publishLocked(protocol.TopicMetrics, protocol.TopicMetrics, protocol.StateEvent{Kind: protocol.TopicMetrics, Payload: protocol.StatePayload{Metrics: &metrics}})
}
func (b *stateBroker) subscribe(w protocol.StateWatch) (*stateSubscriber, *protocol.StateSnapshot, error) {
	if err := w.Validate(); err != nil {
		return nil, nil, fmt.Errorf("daemon: subscribe state: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= b.limit {
		return nil, nil, errWatchLimit
	}
	sub := &stateSubscriber{topics: map[string]bool{}, pending: map[string]protocol.StateEvent{}, wake: make(chan struct{}, 1)}
	for _, topic := range w.Topics {
		sub.topics[topic] = true
	}
	b.subscribers[sub] = struct{}{}
	b.notifyActivityLocked()
	if sub.topics[protocol.TopicSessions] {
		select {
		case b.projection <- struct{}{}:
		default:
		}
	}
	return sub, b.snapshotLocked(sub), nil
}
func (b *stateBroker) unsubscribe(sub *stateSubscriber) {
	b.mu.Lock()
	delete(b.subscribers, sub)
	b.notifyActivityLocked()
	sub.pending = nil
	sub.order = nil
	b.mu.Unlock()
}
func (b *stateBroker) publishLocked(topic, key string, event protocol.StateEvent) {
	for sub := range b.subscribers {
		if sub.topics[topic] {
			b.enqueueLocked(sub, key, event)
		}
	}
}
func (b *stateBroker) enqueueLocked(sub *stateSubscriber, key string, event protocol.StateEvent) {
	if !sub.resync {
		b.queueLocked(sub, key, event)
	}
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}
func (b *stateBroker) queueLocked(sub *stateSubscriber, key string, event protocol.StateEvent) {
	_, exists := sub.pending[key]
	if !exists && len(sub.pending) == stateQueueCapacity {
		clear(sub.pending)
		sub.order = nil
		sub.resync = true
		return
	}
	if !exists {
		sub.order = append(sub.order, key)
	}
	sub.pending[key] = event
}
func (b *stateBroker) take(sub *stateSubscriber) []protocol.Control {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[sub]; !ok {
		return nil
	}
	if sub.resync {
		sub.resync = false
		sub.confirm = false
		return []protocol.Control{{Type: protocol.TypeStateResync, Reason: "subscriber queue overflow"}, {Type: protocol.TypeStateSnapshot, StateSnapshot: b.snapshotLocked(sub)}}
	}
	result := make([]protocol.Control, 0, len(sub.order))
	for _, key := range sub.order {
		event := cloneState(sub.pending[key])
		if event.Kind == protocol.TopicMetrics && event.Payload.Metrics != nil {
			ageMetrics(event.Payload.Metrics, max(0, b.now().Sub(b.metricsAt).Milliseconds()))
		}
		if event.Kind == "session.memory" {
			for id, reading := range event.Payload.Memory {
				reading.AgeMillis += max(0, b.now().Sub(b.memoryAt).Milliseconds())
				event.Payload.Memory[id] = reading
			}
		}
		sub.seq++
		event.Seq = sub.seq
		result = append(result, protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &event, StateCurrent: b.currentLocked(sub)})
	}
	if sub.confirm {
		sub.confirm = false
		sub.seq++
		result = append(result, protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: b.currentLocked(sub)})
	}
	clear(sub.pending)
	sub.order = nil
	return result
}
func (b *stateBroker) snapshotLocked(sub *stateSubscriber) *protocol.StateSnapshot {
	sub.seq++
	snapshot := &protocol.StateSnapshot{Seq: sub.seq, Current: b.currentLocked(sub).Sections}
	if sub.topics[protocol.TopicSessions] {
		snapshot.Memory = cloneState(b.memory)
		for id, reading := range snapshot.Memory {
			reading.AgeMillis += max(0, b.now().Sub(b.memoryAt).Milliseconds())
			snapshot.Memory[id] = reading
		}
		for _, row := range b.sessions {
			snapshot.Sessions = append(snapshot.Sessions, cloneState(row))
		}
		slices.SortFunc(snapshot.Sessions, func(a, c protocol.SessionInfo) int { return strings.Compare(a.ID, c.ID) })
	}
	if sub.topics[protocol.TopicServices] {
		for _, row := range b.services {
			snapshot.Services = append(snapshot.Services, cloneState(row))
		}
		slices.SortFunc(snapshot.Services, func(a, c protocol.ServiceInfo) int { return strings.Compare(a.Name, c.Name) })
	}
	if sub.topics[protocol.TopicMetrics] && b.metrics != nil {
		v := *b.metrics
		ageMetrics(&v, max(0, b.now().Sub(b.metricsAt).Milliseconds()))
		snapshot.Metrics = &v
	}
	return snapshot
}
func (b *stateBroker) current(sub *stateSubscriber) *protocol.StateCurrent {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub.seq++
	return b.currentLocked(sub)
}
func (b *stateBroker) currentLocked(sub *stateSubscriber) *protocol.StateCurrent {
	sections := map[string]protocol.Observation{}
	for _, topic := range []string{protocol.TopicSessions, protocol.TopicServices} {
		if !sub.topics[topic] {
			continue
		}
		o := b.observations[topic]
		age := int64(30000)
		if !o.at.IsZero() {
			age = max(0, b.now().Sub(o.at).Milliseconds())
		}
		sections[topic] = protocol.Observation{AgeMillis: age, Failing: o.failing || o.at.IsZero()}
	}
	return &protocol.StateCurrent{Seq: sub.seq, Sections: sections}
}
func ageMetrics(v *hostmetrics.Snapshot, age int64) {
	*v = hostmetrics.AgeSnapshot(*v, age)
}
func cloneState[T any](value T) T {
	data, _ := json.Marshal(value)
	var cloned T
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func (b *stateBroker) hasTopic(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subscribers {
		if sub.topics[topic] {
			return true
		}
	}
	return false
}
func (b *stateBroker) memoryRows() []storage.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := make([]storage.Session, 0, len(b.stored))
	for _, row := range b.stored {
		rows = append(rows, cloneStoredSession(row))
	}
	return rows
}
func (b *stateBroker) memoryChanged(sizes map[string]uint64, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	memory := make(map[string]protocol.SessionMemory, len(b.sessions))
	for id := range b.sessions {
		bytes, available := sizes[id]
		memory[id] = protocol.SessionMemory{Bytes: bytes, Available: available}
	}
	b.memory = memory
	b.memoryAt = at
	b.publishLocked(protocol.TopicSessions, "session.memory", protocol.StateEvent{Kind: "session.memory", Payload: protocol.StatePayload{Memory: memory}})
}

func (b *stateBroker) notifyActivityLocked() {
	select {
	case b.activity <- struct{}{}:
	default:
	}
}
func (b *stateBroker) seedSessions(rows map[storage.SessionID]storage.Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, row := range rows {
		b.stored[id] = cloneStoredSession(row)
		b.markProjectionLocked(id)
		info := sessionInfo(row)
		info.RecoveryPending = true
		info.RecoveryDetailsOmitted = true
		b.sessions[string(id)] = info
	}
}

func (b *stateBroker) updateObservationLocked(topic string, next sectionObservation) {
	previous := b.observations[topic]
	b.observations[topic] = next
	if previous.failing == next.failing && previous.at.IsZero() == next.at.IsZero() {
		return
	}
	for sub := range b.subscribers {
		if sub.topics[topic] {
			sub.confirm = true
			select {
			case sub.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (b *stateBroker) markProjectionLocked(id storage.SessionID) {
	if _, exists := b.stored[id]; !exists {
		return
	}
	b.revision++
	b.revisions[id] = b.revision
	b.dirty[id] = true
	select {
	case b.projection <- struct{}{}:
	default:
	}
}

func retainRecognition(info *protocol.SessionInfo, previous protocol.SessionInfo) {
	info.Recovery = previous.Recovery
	info.RecoveryDetailsOmitted = previous.RecoveryDetailsOmitted
	info.RecoveryError = previous.RecoveryError
	info.ReplacementID = previous.ReplacementID
	info.RecoveredFrom = previous.RecoveredFrom
	info.AgentStatus = previous.AgentStatus
	info.Hibernated = previous.Hibernated
	info.Label = previous.Label
}

func (b *stateBroker) markRecognitionLocked(id storage.SessionID) {
	b.markProjectionLocked(id)
	for key, row := range b.sessions {
		if row.RecoveredFrom == string(id) || row.ReplacementID == string(id) {
			b.markProjectionLocked(storage.SessionID(key))
		}
	}
}
