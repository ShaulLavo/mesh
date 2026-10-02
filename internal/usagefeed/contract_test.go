package usagefeed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnchangedBodiesParseOnceAndRecoverFromTransportFailure(t *testing.T) {
	data := fixture(t)
	clock := &testClock{at: time.Date(2026, 10, 2, 10, 20, 0, 0, time.UTC)}
	status := http.StatusOK
	f, err := New(Config{URL: "https://feed.example.test/v1.json", Now: clock.now, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { r := response(data); r.StatusCode = status; return r, nil })}})
	if err != nil {
		t.Fatal(err)
	}
	first := f.Refresh(t.Context())
	for range 3 {
		clock.advance(MinInterval)
		f.Refresh(t.Context())
	}
	if f.decodes != 1 || feedSnapshot(f).Revision != first.Revision {
		t.Fatal("unchanged feed was reparsed")
	}
	status = 500
	clock.advance(MinInterval)
	if !f.Refresh(t.Context()).Failing {
		t.Fatal("HTTP failure lost")
	}
	status = 200
	clock.advance(MinInterval)
	if f.Refresh(t.Context()).Failing || f.decodes != 1 {
		t.Fatal("unchanged last-good did not recover")
	}
	data = "{"
	clock.advance(MinInterval)
	if !f.Refresh(t.Context()).Failing {
		t.Fatal("malformed snapshot accepted")
	}
	clock.advance(MinInterval)
	f.Refresh(t.Context())
	if f.decodes != 2 {
		t.Fatal("unchanged malformed body reparsed")
	}
}

func TestStrictV1Values(t *testing.T) {
	good := fixture(t)
	for name, data := range map[string]string{
		"empty":               "",
		"null":                "null",
		"missing-generated":   strings.Replace(good, `"generatedAt": "2026-10-02T10:20:00Z",`, "", 1),
		"unknown-field":       strings.Replace(good, `"schemaVersion": 1,`, `"schemaVersion": 1, "secret": "redacted",`, 1),
		"bad-source":          strings.Replace(good, `"source": "proxy-state"`, `"source": "untrusted"`, 1),
		"control-label":       strings.Replace(good, `"label": "First account"`, `"label": "bad\u001b[31m"`, 1),
		"bidi-label":          strings.Replace(good, `"label": "First account"`, `"label": "bad\u202e"`, 1),
		"bad-routing":         strings.Replace(good, `"mode": "rotating"`, `"mode": "active-now"`, 1),
		"bad-cooldown":        strings.Replace(good, `"reason": "quota"`, `"reason": "raw upstream error"`, 1),
		"missing-windows":     strings.Replace(good, `"windows": [],`, "", 1),
		"missing-observation": strings.Replace(good, `"lastSeenAt": "2026-10-02T10:19:00Z", "source"`, `"lastSeenAt": null, "source"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := newTestFetcher(t, data)
			if result := f.Refresh(t.Context()); result.Err == nil || result.Snapshot != nil {
				t.Fatalf("accepted invalid v1: %+v", result)
			}
		})
	}
}

func TestRunUsesNoRequestsAfterCancellation(t *testing.T) {
	f, _, calls := newTestFetcher(t, fixture(t))
	ctx, cancel := context.WithCancel(t.Context())
	if err := f.Run(ctx, func(result Result) {
		if result.Err != nil {
			t.Error(result.Err)
		}
		cancel()
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests after cancellation: %d", calls.Load())
	}
}

func TestDefaultClientVerifiesTLS(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); _, _ = w.Write([]byte(fixture(t))) }))
	defer server.Close()
	f, err := New(Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if result := f.Refresh(t.Context()); result.Err == nil || requests.Load() != 0 {
		t.Fatal("untrusted TLS reached handler")
	}
	trusted, err := New(Config{URL: server.URL, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if result := trusted.Refresh(t.Context()); result.Err != nil || requests.Load() != 1 {
		t.Fatalf("trusted TLS positive control: %+v", result)
	}
}
