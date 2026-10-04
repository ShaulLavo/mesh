package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestUpdateCheckUsesReleaseDownloadBudget(t *testing.T) {
	setupUpdateCLI(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		updateBudgetHandler(w, r)
	}))
	t.Cleanup(server.Close)
	client := release.Client{BaseURL: server.URL, HTTPClient: server.Client()}
	caller := updateCallFunc(func(_ context.Context, host update.Host, _ string, _, output any) error {
		*output.(*update.Info) = update.Info{AcceptsIdentityFleet: true, Health: updateinstall.Health{HostID: host.ID, Build: release.Build{Version: "v0.1.0"}}}
		return nil
	})
	_, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--version", "v0.2.0", "--check", "--json")
	if err != nil {
		t.Fatalf("update check rejected a manifest within the release budget: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("update check made %d requests, want one timeout and one recovery", calls.Load())
	}
}

func updateBudgetHandler(w http.ResponseWriter, r *http.Request) {
	// MagicDNS stalls have exceeded the former five-second CLI deadline.
	timer := time.NewTimer(5100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		// Returning here would publish an empty 200 response after cancellation.
		panic(http.ErrAbortHandler)
	case <-timer.C:
		_ = json.NewEncoder(w).Encode(updateTestManifest())
	}
}

func TestUpdateBudgetFixtureAbortsCancelledRequest(t *testing.T) {
	var cancelRequest atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cancelRequest.Load() {
			_ = json.NewEncoder(w).Encode(updateTestManifest())
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		updateBudgetHandler(w, r.WithContext(ctx))
	}))
	t.Cleanup(server.Close)
	client := release.Client{BaseURL: server.URL, HTTPClient: server.Client()}
	if manifest, err := client.Manifest(context.Background(), "v0.2.0"); err != nil || manifest.Version != "v0.2.0" {
		t.Fatalf("known-good budget fixture: version %q, error %v", manifest.Version, err)
	}
	cancelRequest.Store(true)
	_, err := client.Manifest(context.Background(), "v0.2.0")
	var transport *url.Error
	if !errors.As(err, &transport) || !errors.Is(err, io.EOF) {
		t.Fatalf("cancelled budget handler must fail the exchange, not publish an empty manifest: %v", err)
	}
}

func TestUpdateBudgetKeepsEmptyManifestTerminal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	client := release.Client{BaseURL: server.URL, HTTPClient: server.Client()}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	var transport *url.Error
	if !errors.Is(err, io.EOF) || errors.As(err, &transport) || calls.Load() != 1 {
		t.Fatalf("empty manifest must fail validation without retry: error %v, requests %d", err, calls.Load())
	}
}
