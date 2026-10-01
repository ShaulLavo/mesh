package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

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
	if err := request.Validate(); err != nil {
		return fmt.Errorf("watch host %s: %w", host.Alias, err)
	}
	view := StateView{Sections: map[string]ObservedSection{}}
	attempt := 0
	for ctx.Err() == nil {
		err := w.watchOnce(ctx, host, request, &view, publish)
		if ctx.Err() != nil {
			break
		}
		if err == nil {
			attempt = 0
			continue
		}
		if errors.Is(err, ErrStateGap) {
			attempt = 0
			continue
		}
		if view.Seq != 0 {
			for _, topic := range request.Topics {
				markSectionFailed(&view, topic)
			}
			publish(view.Clone())
		}
		attempt++
		if err := waitState(ctx, stateBackoff(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("state watch ended: %w", ctx.Err())
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
	setupCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := w.acquire(setupCtx); err != nil {
		return err
	}
	conn, info, err := openVerifiedHostInfo(setupCtx, host, w.dial)
	<-w.reads
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	return w.watchConnected(ctx, setupCtx, host, info, conn, request, view, publish)
}
func (w *StateWatcher) watchConnected(ctx, setupCtx context.Context, host HostRecord, info protocol.HostInfo, conn transport.Conn, request protocol.StateWatch, view *StateView, publish func(StateView)) error {
	build, _ := json.Marshal(info.Build)
	key := info.ID + "/" + info.MeshIdentity
	w.mu.Lock()
	unsupported := w.unsupported[key] == string(build)
	w.mu.Unlock()
	if unsupported {
		return w.poll(ctx, host, info, conn, request, view, publish)
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
		return w.poll(ctx, host, info, conn, request, view, publish)
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
	publish(view.Clone())
	return readStateStream(ctx, host, conn, view, transit, publish)
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
		publish(view.Clone())
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
}

func (w *StateWatcher) poll(ctx context.Context, host HostRecord, _ protocol.HostInfo, conn transport.Conn, request protocol.StateWatch, view *StateView, publish func(StateView)) error {
	sections := make([]pollSection, len(request.Topics))
	for i, topic := range request.Topics {
		sections[i] = pollSection{topic: topic}
	}
	for ctx.Err() == nil {
		next := time.Now().Add(time.Minute)
		for i := range sections {
			due, err := w.pollDue(ctx, host, conn, request, &sections[i], view, publish)
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
func (w *StateWatcher) pollSection(ctx context.Context, host HostRecord, conn transport.Conn, section *pollSection, view *StateView) error {
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
	kind := protocol.TypeList
	if section.topic == protocol.TopicServices {
		kind = protocol.TypeServiceList
	}
	if section.topic == protocol.TopicMetrics {
		kind = protocol.TypeHostMetrics
	}
	started := time.Now()
	response, err := controlRequest(readCtx, conn, protocol.Control{Type: kind, RequestID: id})
	if err != nil {
		return err
	}
	received := time.Now()
	if explicitUnknownControl(response, kind) {
		section.unsupported = true
		markSectionFailed(view, section.topic)
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
	section.failures = 0
	return nil
}
func markSectionFailed(view *StateView, topic string) {
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
		view.applyMetrics(response.Metrics, received, transit)
	}
	view.Sections[topic] = ObservedSection{Observation: protocol.Observation{AgeMillis: max(0, transit.Milliseconds())}, ReceivedAt: received}
	return nil
}

func (w *StateWatcher) pollDue(ctx context.Context, host HostRecord, conn transport.Conn, request protocol.StateWatch, section *pollSection, view *StateView, publish func(StateView)) (time.Time, error) {
	if section.unsupported {
		return time.Now().Add(time.Minute), nil
	}
	if time.Now().Before(section.due) {
		return section.due, nil
	}
	if err := w.pollSection(ctx, host, conn, section, view); err != nil {
		return time.Time{}, err
	}
	view.LastReply = time.Now()
	if err := ctx.Err(); err != nil {
		return time.Time{}, fmt.Errorf("publish state poll: %w", err)
	}
	publish(view.Clone())
	interval := 10 * time.Second
	if section.topic == protocol.TopicMetrics {
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
