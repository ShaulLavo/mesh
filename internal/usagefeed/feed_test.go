package usagefeed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(data string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(data)), Header: make(http.Header)}
}
func newTestFetcher(t *testing.T, body string) (*Fetcher, *testClock, *atomic.Int32) {
	t.Helper()
	clock := &testClock{at: time.Date(2026, 10, 2, 10, 20, 0, 0, time.UTC)}
	calls := &atomic.Int32{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != "http://feed.example.test/static/v1.json" {
			t.Errorf("unexpected passive request: %s %s", r.Method, r.URL)
		}
		return response(body), nil
	})}
	f, err := New(Config{URL: "http://feed.example.test/static/v1.json", Client: client, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return f, clock, calls
}

func TestFixturePreservesGenericWindowsAndNulls(t *testing.T) {
	f, _, calls := newTestFetcher(t, fixture(t))
	result := f.Refresh(context.Background())
	if result.Err != nil || result.Failing || calls.Load() != 1 {
		t.Fatalf("fetch failed: %+v", result)
	}
	s := result.Snapshot
	if s.SchemaVersion != 1 || len(s.Accounts) != 3 {
		t.Fatalf("snapshot: %+v", s)
	}
	account := s.Accounts[0]
	if account.Provider != "opaque-provider" || account.Routing.Active != nil || *account.Windows[0].WindowMinutes != 10080 || *account.Windows[0].UsedPercent != 42 {
		t.Fatalf("account changed: %+v", account)
	}
	if account.Windows[1].UsedPercent != nil || account.Windows[1].ResetsAt != nil || s.Accounts[1].CheckedAt != nil || s.Accounts[1].Routing.Active == nil || *s.Accounts[1].Routing.Active {
		t.Fatal("unknowns/false were not preserved")
	}
	if s.Accounts[2].Cooldown == nil || s.Accounts[2].Cooldown.Reason != "quota" {
		t.Fatal("cooldown lost")
	}
	if !account.Windows[0].LastSeenAt.Equal(time.Date(2026, 10, 2, 10, 19, 0, 0, time.UTC)) {
		t.Fatal("observation rewritten")
	}
}

func TestMinimumCadenceAndDetachedSnapshots(t *testing.T) {
	f, clock, calls := newTestFetcher(t, fixture(t))
	if feedSnapshot(f).Snapshot != nil || calls.Load() != 0 {
		t.Fatal("construction/snapshot fetched")
	}
	first := f.Refresh(context.Background())
	expected := feedSnapshot(f)
	first.Snapshot.Accounts[0].Label = "changed"
	*first.Snapshot.Accounts[0].Windows[0].UsedPercent = 99
	*first.Snapshot.Accounts[0].CheckedAt = time.Time{}
	*first.Snapshot.Accounts[1].Routing.Active = true
	first.Snapshot.Accounts[2].Cooldown.Reason = "changed"
	for range 100 {
		_ = feedSnapshot(f)
		_ = f.Refresh(context.Background())
	}
	if calls.Load() != 1 || !reflect.DeepEqual(expected, feedSnapshot(f)) {
		t.Fatal("consumers altered cache or triggered requests")
	}
	clock.advance(59 * time.Second)
	f.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatal("fetched before 60 seconds")
	}
	clock.advance(time.Second)
	if result := f.Refresh(context.Background()); result.Err != nil || calls.Load() != 2 {
		t.Fatalf("60-second fetch: %+v, calls=%d", result, calls.Load())
	}
}

func TestFailuresRetainWholeLastGoodSnapshot(t *testing.T) {
	good := fixture(t)
	cases := map[string]string{
		"unsupported":       strings.Replace(good, `"schemaVersion": 1`, `"schemaVersion": 2`, 1),
		"malformed":         `{`,
		"trailing":          good + ` {}`,
		"oversized":         good + strings.Repeat(" ", MaxBytes),
		"percentage":        strings.Replace(good, `"usedPercent": 42`, `"usedPercent": 101`, 1),
		"negative":          strings.Replace(good, `"usedPercent": 42`, `"usedPercent": -1`, 1),
		"duration":          strings.Replace(good, `"windowMinutes": 10080`, `"windowMinutes": 0`, 1),
		"timestamp":         strings.Replace(good, `"2026-10-02T10:19:00Z"`, `"yesterday"`, 1),
		"duplicate-account": strings.Replace(good, `"account-two"`, `"account-one"`, 1),
		"duplicate-window":  strings.Replace(good, `"model-window"`, `"primary"`, 1),
		"account-state":     strings.Replace(good, `"state": "ready"`, `"state": "made-up"`, 1),
		"window-status":     strings.Replace(good, `"status": "allowed"`, `"status": "made-up"`, 1),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{at: time.Now()}
			data := good
			f, err := New(Config{URL: "http://feed.example.test/v1.json", Now: clock.now, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(data), nil })}})
			if err != nil {
				t.Fatal(err)
			}
			retained := f.Refresh(context.Background()).Snapshot
			data = bad
			clock.advance(MinInterval)
			result := f.Refresh(context.Background())
			if result.Err == nil || !result.Failing || !reflect.DeepEqual(retained, result.Snapshot) {
				t.Fatalf("failure did not preserve snapshot: %+v", result)
			}
			data = good
			clock.advance(MinInterval)
			result = f.Refresh(context.Background())
			if result.Err != nil || result.Failing {
				t.Fatalf("failed to recover: %+v", result)
			}
		})
	}
}

func TestTransportFailureRetainsSnapshotAndLimitsRetry(t *testing.T) {
	data := fixture(t)
	clock := &testClock{at: time.Now()}
	var calls int
	failing := false
	f, err := New(Config{URL: "http://feed.example.test/v1.json", Now: clock.now, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if failing {
			return nil, errors.New("offline")
		}
		return response(data), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	retained := f.Refresh(context.Background()).Snapshot
	failing = true
	clock.advance(MinInterval)
	result := f.Refresh(context.Background())
	if result.Err == nil || !result.Failing || !reflect.DeepEqual(result.Snapshot, retained) {
		t.Fatal("transport failure discarded data")
	}
	f.Refresh(context.Background())
	if calls != 2 {
		t.Fatal("failure retried immediately")
	}
}

func TestCancellationAndBoundedDeadline(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "caller"}[cancelCaller], func(t *testing.T) {
			started := make(chan struct{})
			ended := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(ended) }))
			defer server.Close()
			timeout := 30 * time.Millisecond
			if cancelCaller {
				timeout = time.Second
			}
			f, err := New(Config{URL: server.URL + "/v1.json", Timeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() { done <- f.Refresh(ctx) }()
			<-started
			if cancelCaller {
				cancel()
			}
			select {
			case result := <-done:
				want := context.DeadlineExceeded
				if cancelCaller {
					want = context.Canceled
				}
				if !errors.Is(result.Err, want) || !result.Failing {
					t.Fatalf("cancellation: %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fetch did not stop")
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("HTTP context not canceled")
			}
		})
	}
}

func TestOneRequestInFlightAndCadenceAfterCompletion(t *testing.T) {
	data := fixture(t)
	clock := &testClock{at: time.Now()}
	started := make(chan struct{})
	release := make(chan struct{})
	calls := atomic.Int32{}
	f, err := New(Config{URL: "http://feed.example.test/v1.json", Now: clock.now, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-release
		return response(data), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() { done <- f.Refresh(context.Background()) }()
	<-started
	clock.advance(2 * MinInterval)
	var readers sync.WaitGroup
	for range 20 {
		readers.Go(func() { f.Refresh(context.Background()); feedSnapshot(f) })
	}
	readers.Wait()
	if calls.Load() != 1 {
		t.Fatal("parallel requests")
	}
	close(release)
	result := <-done
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if !result.NextFetchAt.Equal(clock.now().Add(MinInterval)) {
		t.Fatal("cadence starts before completion")
	}
	f.Refresh(context.Background())
	if calls.Load() != 1 {
		t.Fatal("fetch immediately after slow response")
	}
}

func TestRejectsRedirectsAndHTTPFailures(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, status := range []int{302, 401, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL+"/ai")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private error body"))
			}))
			defer server.Close()
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
			f, err := New(Config{URL: server.URL + "/v1.json", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			result := f.Refresh(context.Background())
			if result.Err == nil || !result.Failing || strings.Contains(result.Err.Error(), "private error body") {
				t.Fatalf("HTTP failure: %+v", result)
			}
		})
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("followed redirect to activation route")
	}
}

func TestURLAndTimeoutValidation(t *testing.T) {
	for _, url := range []string{"", "file:///feed.json", "http:///feed.json", "https://user:secret@feed.example.test/v1.json", "https://feed.example.test/v1.json?token=secret", "https://feed.example.test/v1.json#fragment"} {
		if _, err := New(Config{URL: url}); err == nil {
			t.Errorf("accepted invalid URL %q", url)
		}
	}
	if _, err := New(Config{URL: "http://feed.example.test/v1.json", Timeout: time.Minute}); err == nil {
		t.Fatal("accepted unbounded deadline")
	}
}

func feedSnapshot(f *Fetcher) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copyResult()
}
