package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
)

var errStateBuildChanged = errors.New("host build changed during state polling")

// StateWatcher shares admission and capability observations across watched hosts.
// Dial must be the CLI's DialControl, which uses transport.DialOnce without recovery.
type StateWatcher struct {
	dial        HostDialer
	reads       chan struct{}
	mu          sync.Mutex
	unsupported map[string]string
}

func NewStateWatcher(dialControl HostDialer) *StateWatcher {
	return &StateWatcher{dial: dialControl, reads: make(chan struct{}, 4), unsupported: map[string]string{}}
}
func (w *StateWatcher) Watch(ctx context.Context, host HostRecord, request protocol.StateWatch, publish func(StateView)) error {
	return w.watch(ctx, host, request, func(view StateView) { publish(view.Clone()) })
}

// watch lends reader-owned state for synchronous projection; the callback must not retain it.
func (w *StateWatcher) watch(ctx context.Context, host HostRecord, request protocol.StateWatch, publish func(StateView)) error {
	if err := request.Validate(); err != nil {
		return fmt.Errorf("watch host %s: %w", host.Alias, err)
	}
	view := StateView{Sections: map[string]ObservedSection{}}
	attempt := 0
	for ctx.Err() == nil {
		err := w.watchAttempt(ctx, host, request, &view, publish, &attempt)
		if ctx.Err() != nil {
			break
		}
		if err == nil {
			attempt = 0
			continue
		}
		if retryStateGap(err, attempt) {
			attempt++
			continue
		}
		for _, topic := range request.Topics {
			markSectionFailed(&view, topic)
		}
		view.Problem = err.Error()
		publish(view)
		attempt++
		if err := waitState(ctx, stateBackoff(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("state watch ended: %w", ctx.Err())
}
func (w *StateWatcher) watchAttempt(ctx context.Context, host HostRecord, request protocol.StateWatch, view *StateView, publish func(StateView), attempt *int) error {
	publications := 0
	var initialSeq uint64
	return w.watchOnce(ctx, host, request, view, func(next StateView) {
		if publications > 0 && !next.LastReply.IsZero() && (!next.initialized || next.Seq > initialSeq) {
			*attempt = 0
		}
		if publications == 0 {
			initialSeq = next.Seq
		}
		publications++
		publish(next)
	})
}
func stateBackoff(attempt int) time.Duration {
	return time.Duration(min(60, 5<<min(max(attempt-1, 0), 4))) * time.Second
}
func waitState(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("state watch wait: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
func (w *StateWatcher) acquire(ctx context.Context) error {
	select {
	case w.reads <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("state read admission: %w", ctx.Err())
	}
}
func (w *StateWatcher) watchOnce(ctx context.Context, host HostRecord, request protocol.StateWatch, view *StateView, publish func(StateView)) error {
	view.Build = release.Build{}
	setupCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := w.acquire(setupCtx); err != nil {
		return err
	}
	conn, info, err := openVerifiedHostInfo(setupCtx, host, w.dial)
	<-w.reads
	if err != nil {
		view.Connection = stateConnectionError(err)
		return err
	}
	view.Connection, view.Problem = StateReachable, ""
	view.ServiceHealthSupported = info.ServiceHealthSupported
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	return w.watchConnected(ctx, setupCtx, host, info, conn, request, view, publish)
}
func (w *StateWatcher) watchConnected(ctx, setupCtx context.Context, host HostRecord, info protocol.HostInfo, conn transport.Conn, request protocol.StateWatch, view *StateView, publish func(StateView)) error {
	build, _ := json.Marshal(info.Build)
	key := info.ID + "/" + info.MeshIdentity + "/" + protocol.TypeStateWatch
	w.mu.Lock()
	unsupported := w.unsupported[key] == string(build)
	w.mu.Unlock()
	if unsupported {
		_ = conn.Close()
		return w.poll(ctx, host, info, request, view, publish)
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	started := time.Now()
	if err := w.acquire(setupCtx); err != nil {
		return err
	}
	response, err := controlRequest(setupCtx, conn, protocol.Control{Type: protocol.TypeStateWatch, RequestID: id, Watch: &request})
	<-w.reads
	if err != nil {
		return err
	}
	if explicitUnknownControl(response, protocol.TypeStateWatch) {
		w.mu.Lock()
		w.unsupported[key] = string(build)
		w.mu.Unlock()
		_ = conn.Close()
		return w.poll(ctx, host, info, request, view, publish)
	}
	if response.Type == protocol.TypeError {
		return daemonResponseError("state watch", response.Message)
	}
	if response.Type != protocol.TypeStateSnapshot {
		return errors.New("state watch requires an initial snapshot")
	}
	if err := validateStateHost(host, response); err != nil {
		return err
	}
	received := time.Now()
	transit := received.Sub(started)
	if err := view.Apply(response, received, transit); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish host state: %w", err)
	}
	if info.Build != nil {
		view.Build = *info.Build
	} else {
		view.Build = release.Build{}
	}
	publish(*view)
	err = readStateStream(ctx, host, conn, view, transit, publish)
	if err != nil {
		view.Connection = StateUnreachable
	}
	return err
}

func stateConnectionError(err error) StateConnection {
	var identityErr *hostIdentityError
	if errors.As(err, &identityErr) {
		return StateRefused
	}
	return StateUnreachable
}

func readStateStream(ctx context.Context, host HostRecord, conn transport.Conn, view *StateView, transit time.Duration, publish func(StateView)) error {
	for ctx.Err() == nil {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("read host state: %w", err)
		}
		if frame.Kind != protocol.KindControl {
			return errors.New("state watch received terminal data")
		}
		response, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("read host state: %w", err)
		}
		if err := validateStateHost(host, response); err != nil {
			return err
		}
		received := time.Now()
		if err := view.Apply(response, received, transit); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("publish host state: %w", err)
		}
		publish(*view)
	}
	return nil
}

// Legacy daemons use this exact message. Only the reader boundary grants fallback.
func explicitUnknownControl(response protocol.Control, control string) bool {
	return response.Type == protocol.TypeError && (response.ErrorCode == protocol.ErrorCodeUnknownControl || response.ErrorCode == "") && response.Message == fmt.Sprintf("daemon: unknown control %q", control)
}
func validateStateHost(host HostRecord, response protocol.Control) error {
	var sessions []protocol.SessionInfo
	if response.StateSnapshot != nil {
		sessions = response.StateSnapshot.Sessions
	}
	if response.StateEvent != nil && response.StateEvent.Payload.Session != nil {
		sessions = []protocol.SessionInfo{*response.StateEvent.Payload.Session}
	}
	for _, row := range sessions {
		if err := validateDaemonSession(row); err != nil {
			return err
		}
		if row.HostID != host.ID {
			return errors.New("state watch session belongs to another host")
		}
	}
	return nil
}

type pollSection struct {
	topic       string
	due         time.Time
	failures    int
	unsupported bool
	capability  string
	build       string
}

func (w *StateWatcher) poll(ctx context.Context, host HostRecord, info protocol.HostInfo, request protocol.StateWatch, view *StateView, publish func(StateView)) error {
	sections := make([]pollSection, len(request.Topics))
	for i, topic := range request.Topics {
		build, _ := json.Marshal(info.Build)
		sections[i] = pollSection{topic: topic, capability: info.ID + "/" + info.MeshIdentity + "/" + pollControl(topic), build: string(build)}
	}
	for ctx.Err() == nil {
		next := time.Now().Add(time.Minute)
		for i := range sections {
			due, err := w.pollDue(ctx, host, request, &sections[i], view, publish)
			if err != nil {
				return err
			}
			next = minTime(next, due)
		}
		if err := waitState(ctx, max(time.Millisecond, time.Until(next))); err != nil {
			return err
		}
	}
	return fmt.Errorf("state poll ended: %w", ctx.Err())
}
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func (w *StateWatcher) pollSection(ctx context.Context, host HostRecord, section *pollSection, view *StateView) error {
	view.Build = release.Build{}
	readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := w.acquire(readCtx); err != nil {
		return err
	}
	defer func() { <-w.reads }()
	id, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	kind := pollControl(section.topic)
	conn, info, err := openVerifiedHostInfo(readCtx, host, w.dial)
	if err != nil {
		view.Connection = stateConnectionError(err)
		view.Problem = err.Error()
		return err
	}
	view.Connection, view.Problem = StateReachable, ""
	view.ServiceHealthSupported = info.ServiceHealthSupported
	defer func() { _ = conn.Close() }()
	build, _ := json.Marshal(info.Build)
	if section.build != string(build) {
		return errStateBuildChanged
	}
	w.mu.Lock()
	unsupported := w.unsupported[section.capability] == section.build
	w.mu.Unlock()
	if unsupported {
		markUnsupportedSection(view, section)
		return nil
	}
	started := time.Now()
	response, err := controlRequest(readCtx, conn, protocol.Control{Type: kind, RequestID: id, Lean: true})
	if err != nil {
		return err
	}
	received := time.Now()
	view.LastReply = received
	if explicitUnknownControl(response, kind) {
		w.mu.Lock()
		w.unsupported[section.capability] = section.build
		w.mu.Unlock()
		markUnsupportedSection(view, section)
		return nil
	}
	if response.Type == protocol.TypeError {
		section.failures++
		markSectionFailed(view, section.topic)
		return nil
	}
	if err := applyPolledSection(host, section.topic, response, view, received, received.Sub(started)); err != nil {
		return err
	}
	if info.Build != nil {
		view.Build = *info.Build
	} else {
		view.Build = release.Build{}
	}
	section.failures = 0
	return nil
}
func markSectionFailed(view *StateView, topic string) {
	if view.Sections == nil {
		view.Sections = map[string]ObservedSection{}
	}
	observation := view.Sections[topic]
	observation.Observation.Failing = true
	view.Sections[topic] = observation
	if topic == protocol.TopicMetrics && view.Metrics != nil {
		view.Metrics.CPU.Failing = true
		view.Metrics.RAM.Failing = true
		view.Metrics.Temperature.Failing = true
		view.Metrics.Uptime.Failing = true
	}
}
func applyPolledSection(host HostRecord, topic string, response protocol.Control, view *StateView, received time.Time, transit time.Duration) error {
	switch topic {
	case protocol.TopicSessions:
		if response.Type != protocol.TypeListed {
			return errors.New("unexpected polling session response")
		}
		if err := validatePolledSessions(host, response.Sessions); err != nil {
			return err
		}
		view.Sessions = cloneSessionInfo(response.Sessions)
	case protocol.TopicServices:
		if response.Type != protocol.TypeServiceListed {
			return errors.New("unexpected polling service response")
		}
		view.Services = cloneWireServices(response.Services)
	case protocol.TopicMetrics:
		if response.Type != protocol.TypeHostMetricsResult || response.Metrics == nil {
			return errors.New("unexpected polling metrics response")
		}
		if err := validateStateMetrics(response.Metrics); err != nil {
			return err
		}
		view.applyMetrics(response.Metrics, received, transit)
	}
	view.Sections[topic] = ObservedSection{Observation: protocol.Observation{AgeMillis: max(0, transit.Milliseconds())}, ReceivedAt: received}
	return nil
}

func (w *StateWatcher) pollDue(ctx context.Context, host HostRecord, request protocol.StateWatch, section *pollSection, view *StateView, publish func(StateView)) (time.Time, error) {
	if time.Now().Before(section.due) {
		return section.due, nil
	}
	if err := w.pollSection(ctx, host, section, view); err != nil {
		if ctx.Err() != nil || errors.Is(err, errStateBuildChanged) {
			return time.Time{}, err
		}
		section.failures++
		markSectionFailed(view, section.topic)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, fmt.Errorf("publish state poll: %w", err)
	}
	publish(*view)
	interval := 10 * time.Second
	if section.topic == protocol.TopicMetrics && !section.unsupported {
		interval = request.MetricsEvery()
	}
	if section.failures > 0 {
		interval = stateBackoff(section.failures)
	}
	section.due = time.Now().Add(interval)
	return section.due, nil
}

func validatePolledSessions(host HostRecord, rows []protocol.SessionInfo) error {
	for _, row := range rows {
		if err := validateDaemonSession(row); err != nil {
			return err
		}
		if row.HostID != host.ID {
			return errors.New("polling session belongs to another host")
		}
	}
	return nil
}

func pollControl(topic string) string {
	if topic == protocol.TopicServices {
		return protocol.TypeServiceList
	}
	if topic == protocol.TopicMetrics {
		return protocol.TypeHostMetrics
	}
	return protocol.TypeList
}
func markUnsupportedSection(view *StateView, section *pollSection) {
	section.unsupported = true
	markSectionFailed(view, section.topic)
	if section.topic != protocol.TopicMetrics {
		return
	}
	view.MetricsUnsupported = true
	if view.Metrics == nil {
		view.Metrics = &hostmetrics.Snapshot{}
	}
	view.Metrics.CPU.Availability = hostmetrics.Unsupported
	view.Metrics.RAM.Availability = hostmetrics.Unsupported
	view.Metrics.Temperature.Availability = hostmetrics.Unsupported
	view.Metrics.Uptime.Availability = hostmetrics.Unsupported
	view.Metrics.CPU.Failing = false
	view.Metrics.RAM.Failing = false
	view.Metrics.Temperature.Failing = false
	view.Metrics.Uptime.Failing = false
}

func retryStateGap(err error, attempt int) bool { return attempt == 0 && errors.Is(err, ErrStateGap) }
