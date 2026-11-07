package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/usagefeed"
)

const dashboardObserverHeartbeat = 10 * time.Second

type dashboardFeedRunner interface {
	Run(context.Context, func(usagefeed.Result)) error
}
type dashboardFeedResult struct {
	generation uint64
	result     usagefeed.Result
	httpAt     time.Time
}
type dashboardFeedWatcher struct {
	domainToken         domainpolicy.CatalogToken
	read                func() (update.FeedValue, error)
	fetcher             func(string) (dashboardFeedRunner, error)
	observe             func(context.Context, update.ConfigurationObservation) error
	hostID, instanceID  string
	acknowledgements    chan DashboardUsageAcknowledgement
	interval, heartbeat time.Duration
}

func newDashboardFeedWatcher(localID, socket string) *dashboardFeedWatcher {
	watcher := &dashboardFeedWatcher{read: readDashboardFeed, fetcher: func(url string) (dashboardFeedRunner, error) { return usagefeed.New(usagefeed.Config{URL: url}) }, hostID: localID, acknowledgements: make(chan DashboardUsageAcknowledgement, 1), interval: time.Second, heartbeat: dashboardObserverHeartbeat}
	watcher.instanceID, _ = update.NewAutomaticCommandID()

	watcher.observe = localDashboardObserver(localID, socket)
	return watcher
}
func readDashboardFeed() (update.FeedValue, error) {
	path, err := ConfigPath()
	if err != nil {
		return update.FeedValue{}, err
	}
	dir, err := openClientConfigDirectory(filepath.Dir(path), false)
	if err != nil || dir == nil {
		return update.FeedValue{}, err
	}
	defer func() { _ = dir.Close() }()
	contents, err := readClientConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		return update.FeedValue{}, nil
	}
	if err != nil {
		return update.FeedValue{}, err
	}
	return validatedFeedConfig(contents, path)
}
func localDashboardObserver(localID, socket string) func(context.Context, update.ConfigurationObservation) error {
	stateDir, err := paths.StateDir()
	if err != nil || localID == "" || socket == "" {
		return nil
	}
	host, key, err := identity.LoadPrivate(stateDir)
	if err != nil || host.ID != localID {
		return nil
	}
	client, target := update.Client{ID: host.ID, Key: key}, update.LocalHost(stateDir, host.ID)
	if target.Endpoint != "unix://"+socket {
		return nil
	}
	return func(ctx context.Context, observation update.ConfigurationObservation) error {
		call, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		return client.Call(call, target, "configuration.observe", observation, nil)
	}
}
func (w *dashboardFeedWatcher) acknowledge(ack DashboardUsageAcknowledgement) {
	select {
	case w.acknowledgements <- ack:
	default:
		select {
		case <-w.acknowledgements:
		default:
		}
		select {
		case w.acknowledgements <- ack:
		default:
		}
	}
}

type dashboardFeedSession struct {
	watcher  *dashboardFeedWatcher
	publish  func(DashboardUsageUpdate)
	results  chan dashboardFeedResult
	stop     context.CancelFunc
	done     chan struct{}
	current  DashboardUsageUpdate
	httpAt   time.Time
	accepted *DashboardUsageAcknowledgement
}

func (w *dashboardFeedWatcher) Run(ctx context.Context, publish func(DashboardUsageUpdate)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("dashboard configuration watch: %w", err)
	}
	w.domainToken, _ = loadedServicePolicy(ctx)
	ticker := time.NewTicker(w.interval)
	heartbeat := time.NewTicker(w.heartbeat)
	defer ticker.Stop()
	defer heartbeat.Stop()
	session := dashboardFeedSession{watcher: w, publish: publish, results: make(chan dashboardFeedResult, 1)}
	defer func() { stopDashboardFeed(session.stop, session.done) }()
	if err := session.refresh(ctx); err != nil {
		return err
	}
	for {
		if err := session.step(ctx, ticker.C, heartbeat.C); err != nil {
			return err
		}
	}
}
func (s *dashboardFeedSession) step(ctx context.Context, ticks, heartbeats <-chan time.Time) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("dashboard configuration watch: %w", ctx.Err())
	case <-ticks:
		return s.refresh(ctx)
	case next := <-s.results:
		s.receive(next)
	case ack := <-s.watcher.acknowledgements:
		s.acknowledge(ctx, ack)
	case <-heartbeats:
		s.heartbeat(ctx)
	}
	return nil
}
func (s *dashboardFeedSession) heartbeat(ctx context.Context) {
	if s.accepted != nil {
		s.watcher.renew(ctx, s.current, s.httpAt)
	}
}

func (s *dashboardFeedSession) refresh(ctx context.Context) error {
	next := s.watcher.configuration()
	if s.current.Generation != 0 && next.Digest == s.current.Digest && next.State == s.current.State {
		return nil
	}
	stopDashboardFeed(s.stop, s.done)
	s.stop, s.done = nil, nil
	next.Generation = s.current.Generation + 1
	next.Publication = 1
	s.current, s.accepted, s.httpAt = next, nil, time.Time{}
	s.publish(s.current)
	if !s.current.Enabled {
		return nil
	}
	fetcher, err := s.watcher.fetcher(next.URL)
	if err != nil {
		return fmt.Errorf("dashboard feed setup: %w", err)
	}
	run, stop := context.WithCancel(ctx)
	s.stop, s.done = stop, make(chan struct{})
	go runDashboardFeed(run, fetcher, s.current.Generation, s.results, s.done)
	return nil
}
func (s *dashboardFeedSession) receive(next dashboardFeedResult) {
	if next.generation != s.current.Generation {
		return
	}
	s.current.Result, s.httpAt, s.accepted = next.result, next.httpAt, nil
	s.current.Publication++
	s.publish(s.current)
}
func (s *dashboardFeedSession) acknowledge(ctx context.Context, ack DashboardUsageAcknowledgement) {
	if ack != s.current.Acknowledgement() {
		return
	}
	s.accepted = &ack
	s.watcher.renew(ctx, s.current, s.httpAt)
}
func stopDashboardFeed(stop context.CancelFunc, done chan struct{}) {
	if stop == nil {
		return
	}
	stop()
	<-done
}
func runDashboardFeed(ctx context.Context, fetcher dashboardFeedRunner, generation uint64, results chan<- dashboardFeedResult, done chan struct{}) {
	defer close(done)
	_ = fetcher.Run(ctx, func(result usagefeed.Result) {
		next := dashboardFeedResult{generation, result, time.Now().UTC()}
		select {
		case results <- next:
		case <-ctx.Done():
		}
	})
}
func (w *dashboardFeedWatcher) configuration() DashboardUsageUpdate {
	value, err := w.read()
	state := dashboardFeedDisabled
	if value.URL != "" {
		state = dashboardFeedEnabled
	}
	result := DashboardUsageUpdate{State: state, Enabled: state == dashboardFeedEnabled, Digest: value.Digest(), URL: value.URL}
	if err != nil {
		result.State, result.Enabled, result.URL, result.Digest = dashboardFeedInvalid, false, "", (update.FeedValue{}).Digest()
		result.Result = usagefeed.Result{Failing: true, Err: err}
	}
	return result
}
func (w *dashboardFeedWatcher) renew(ctx context.Context, current DashboardUsageUpdate, httpAt time.Time) {
	if w.observe == nil || w.instanceID == "" {
		return
	}
	observation := update.ConfigurationObservation{DomainDigest: domainpolicy.Current().Digest(), CatalogGeneration: w.domainToken.Generation, ServiceVerification: serviceHealthUnknown, InstanceID: w.instanceID, HostID: w.hostID, Generation: current.Generation, Publication: current.Publication, Enabled: current.Enabled, State: current.State, Digest: current.Digest, FeedRevision: current.Result.Revision, Failing: current.Result.Failing, HTTPAt: httpAt}
	if current.Result.Err != nil {
		observation.Problem = strings.ToValidUTF8(current.Result.Err.Error(), "?")
		if len(observation.Problem) > 512 {
			observation.Problem = observation.Problem[:512]
		}
	}
	if snapshot := current.Result.Snapshot; snapshot != nil {
		observation.PublishedAt = snapshot.GeneratedAt
		for _, account := range snapshot.Accounts {
			observation.UpstreamAt = oldestDashboardTime(observation.UpstreamAt, account.LastSeenAt)
		}
	}
	_ = w.observe(ctx, observation)
}
func oldestDashboardTime(current, next *time.Time) *time.Time {
	if next != nil && (current == nil || next.Before(*current)) {
		copyTime := *next
		return &copyTime
	}
	return current
}
