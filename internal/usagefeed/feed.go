package usagefeed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	MinInterval = time.Minute
	maxTimeout  = 15 * time.Second
)

type Config struct {
	URL     string
	Client  *http.Client
	Now     func() time.Time
	Timeout time.Duration
}

type Result struct {
	Snapshot    *Snapshot
	Failing     bool
	Err         error
	NextFetchAt time.Time
	Revision    uint64
}

// Fetcher serializes reads and owns the last valid snapshot independently of callers.
type Fetcher struct {
	mu        sync.Mutex
	url       string
	client    http.Client
	now       func() time.Time
	timeout   time.Duration
	busy      bool
	result    Result
	body      []byte
	bodyError error
	decodes   uint64
}

func ValidateURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return fmt.Errorf("usage feed URL needs an HTTP or HTTPS origin and path without credentials, query or fragment")
	}
	return nil
}

func New(config Config) (*Fetcher, error) {
	if err := ValidateURL(config.URL); err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > maxTimeout {
		return nil, fmt.Errorf("usage feed timeout must be positive and at most 15 seconds")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	client := http.Client{}
	if config.Client != nil {
		client = *config.Client
	}
	// Even an injected client's redirect policy must not activate another route.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Fetcher{url: config.URL, client: client, now: config.Now, timeout: config.Timeout}, nil
}

func (f *Fetcher) copyResult() Result {
	result := f.result
	result.Snapshot = clone(result.Snapshot)
	return result
}

func (f *Fetcher) Refresh(ctx context.Context) Result {
	f.mu.Lock()
	if f.busy || f.now().Before(f.result.NextFetchAt) {
		result := f.copyResult()
		f.mu.Unlock()
		return result
	}
	f.busy = true
	f.mu.Unlock()
	data, err := f.read(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = false
	f.result.NextFetchAt = f.now().Add(MinInterval)
	if err == nil {
		err = f.accept(data)
	}
	f.result.Err, f.result.Failing = err, err != nil
	return f.copyResult()
}

func (f *Fetcher) accept(data []byte) error {
	if f.body != nil && bytes.Equal(data, f.body) {
		return f.bodyError
	}
	f.body = data
	f.decodes++
	snapshot, err := decode(data)
	f.bodyError = err
	if err == nil {
		f.result.Snapshot = snapshot
		f.result.Revision++
	}
	return err
}

func (f *Fetcher) read(ctx context.Context) ([]byte, error) {
	run, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(run, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, fmt.Errorf("usage feed request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := f.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("usage feed request: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // bounded read outcome is authoritative
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage feed returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > MaxBytes {
		return nil, fmt.Errorf("usage feed exceeds 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("usage feed read: %w", err)
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("usage feed exceeds 64 KiB")
	}
	return data, nil
}

// Run starts its timer after completion, so slow responses cannot overlap or catch up.
func (f *Fetcher) Run(ctx context.Context, publish func(Result)) error {
	for ctx.Err() == nil {
		result := f.Refresh(ctx)
		if ctx.Err() != nil {
			break
		}
		publish(result)
		timer := time.NewTimer(max(MinInterval, result.NextFetchAt.Sub(f.now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("usage feed watch: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("usage feed watch: %w", ctx.Err())
}
