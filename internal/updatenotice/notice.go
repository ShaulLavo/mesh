// Package updatenotice caches release availability independently of attachment.
package updatenotice

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/release"
)

const (
	checkTimeout  = 5 * time.Second
	checkInterval = 6 * time.Hour
	retryInterval = 15 * time.Minute
)

type Config struct {
	Directory string
	Current   release.Build
	Client    release.Client
}

type Notice struct {
	Version   string    `json:"version,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

type dismissal struct {
	Skip  bool      `json:"skip,omitempty"`
	Until time.Time `json:"until,omitempty"`
}

type cacheState struct {
	Version     string               `json:"version,omitempty"`
	CheckedAt   time.Time            `json:"checkedAt,omitempty"`
	AttemptedAt time.Time            `json:"attemptedAt,omitempty"`
	NextAttempt time.Time            `json:"nextAttempt,omitempty"`
	Dismissals  map[string]dismissal `json:"dismissals,omitempty"`
}

type Store struct {
	directory string
	current   release.Build
	fetch     func(context.Context) (release.Manifest, error)
	now       func() time.Time
	jitter    func() time.Duration
}

// New neither creates files nor contacts the release server.
func New(config Config) *Store {
	store := &Store{
		directory: config.Directory,
		current:   config.Current,
		now:       time.Now,
		jitter:    func() time.Duration { return time.Duration(rand.Int64N(int64(30 * time.Minute))) }, //nolint:gosec // scheduling jitter is not a security value
	}
	client := config.Client
	httpClient := http.Client{}
	if client.HTTPClient != nil {
		httpClient = *client.HTTPClient
	}
	httpClient.Transport = &conditionalTransport{base: httpClient.Transport, directory: config.Directory}
	client.HTTPClient = &httpClient
	store.fetch = func(ctx context.Context) (release.Manifest, error) { return client.Manifest(ctx, "latest") }
	return store
}

// Cached only reads local state. An unavailable cache is not an update offer.
func (s *Store) Cached() (Notice, error) {
	state, err := s.read()
	if err != nil {
		return Notice{}, err
	}
	return s.notice(state), nil
}

// Due checks the persisted deadline without creating files or contacting a host.
func (s *Store) Due() (bool, error) {
	state, err := s.read()
	return !s.now().Before(state.NextAttempt), err
}

func (s *Store) notice(state cacheState) Notice {
	result := Notice{CheckedAt: state.CheckedAt}
	comparison, err := release.CompareVersions(state.Version, s.current.Version)
	if err != nil || comparison <= 0 {
		return result
	}
	dismissed := state.Dismissals[state.Version]
	if dismissed.Skip || s.now().Before(dismissed.Until) {
		return result
	}
	result.Version = state.Version
	return result
}

// Refresh has a five-second total deadline. Another process already refreshing
// the cache wins; the caller immediately receives the previous cached result.
func (s *Store) Refresh(ctx context.Context, force bool) (Notice, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	unlock, err := s.lock(ctx, "refresh.lock", false)
	if errors.Is(err, errLocked) {
		return s.Cached()
	}
	if err != nil {
		return Notice{}, err
	}
	defer unlock()
	state, err := s.read()
	if err != nil {
		return Notice{}, err
	}
	if !force && s.now().Before(state.NextAttempt) {
		return s.notice(state), nil
	}
	if err := s.recordAttempt(ctx); err != nil {
		return s.notice(state), err
	}
	manifest, err := s.fetch(ctx)
	if err != nil {
		return s.notice(state), err
	}
	if err := manifest.Validate(); err != nil {
		return s.notice(state), err
	}
	err = s.Record(ctx, manifest)
	if err != nil {
		return s.notice(state), err
	}
	return s.Cached()
}

// Record shares an explicit check's verified manifest with later interactive
// invocations. Callers obtain the descriptor through release.Client.Manifest.
func (s *Store) Record(ctx context.Context, manifest release.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	return s.change(ctx, func(current *cacheState) {
		current.Version = manifest.Version
		current.CheckedAt = s.now()
		current.NextAttempt = current.CheckedAt.Add(checkInterval + s.jitter())
	})
}

func (s *Store) recordAttempt(ctx context.Context) error {
	return s.change(ctx, func(state *cacheState) {
		state.AttemptedAt = s.now()
		state.NextAttempt = state.AttemptedAt.Add(retryInterval)
	})
}

// Dismiss records a version-specific skip, or a reminder after 24 hours.
func (s *Store) Dismiss(ctx context.Context, version string, skip bool) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if _, err := release.CompareVersions(version, version); err != nil {
		return fmt.Errorf("dismiss update: %w", err)
	}
	return s.change(ctx, func(state *cacheState) {
		if state.Dismissals == nil {
			state.Dismissals = make(map[string]dismissal)
		}
		state.Dismissals[version] = dismissal{Skip: skip, Until: s.now().Add(24 * time.Hour)}
	})
}

// Run is the daemon's optional background checker. Failures retain the last
// successful check and are retried after the persisted throttle deadline.
func (s *Store) Run(ctx context.Context) {
	for ctx.Err() == nil {
		_, _ = s.Refresh(ctx, false)
		if !s.wait(ctx) {
			return
		}
	}
}

func (s *Store) wait(ctx context.Context) bool {
	delay := retryInterval
	if state, err := s.read(); err == nil && !state.NextAttempt.IsZero() {
		delay = max(time.Minute, state.NextAttempt.Sub(s.now()))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Store) read() (cacheState, error) {
	var state cacheState
	if s.directory == "" {
		return state, errors.New("update notice cache directory is required")
	}
	err := readJSON(filepath.Join(s.directory, "notice.json"), &state)
	return state, err
}

func (s *Store) change(ctx context.Context, mutate func(*cacheState)) error {
	unlock, err := s.lock(ctx, "state.lock", true)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.read()
	if err != nil {
		return err
	}
	mutate(&state)
	return writeJSON(filepath.Join(s.directory, "notice.json"), state)
}
