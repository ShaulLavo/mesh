package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManifestRetriesSlowResolver(t *testing.T) {
	contents, _ := json.Marshal(testManifest())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(contents)
	}))
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	var calls atomic.Int32
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		if calls.Add(1) != 1 {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		return (&net.Dialer{Resolver: resolver, Timeout: 100 * time.Millisecond}).DialContext(ctx, network, "slow.invalid:443")
	}
	client := Client{BaseURL: "https://example.com", HTTPClient: &http.Client{Transport: transport, Timeout: 100 * time.Millisecond}}
	manifest, err := client.Manifest(context.Background(), "v0.2.0")
	if err != nil || manifest.Version != "v0.2.0" || calls.Load() != 2 {
		t.Fatalf("Manifest() = %q, %v; dials %d; want recovered manifest after two dials", manifest.Version, err, calls.Load())
	}
}

func TestDownloadTimeoutNamesStageAndBoundsAttempts(t *testing.T) {
	for _, stage := range []string{"DNS", "connect", "read"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			transport := downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				trace := httptrace.ContextClientTrace(request.Context())
				if trace != nil {
					startDownloadStage(trace, stage)
				}
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
			client := Client{HTTPClient: &http.Client{Transport: transport, Timeout: 10 * time.Millisecond}}
			_, err := client.Manifest(context.Background(), "v0.2.0")
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), stage) || calls.Load() != 3 {
				t.Fatalf("Manifest() error = %v, calls %d; want %s deadline after 3 attempts", err, calls.Load(), stage)
			}
		})
	}
}

func startDownloadStage(trace *httptrace.ClientTrace, stage string) {
	switch stage {
	case "DNS":
		trace.DNSStart(httptrace.DNSStartInfo{Host: "release.invalid"})
	case "connect":
		trace.ConnectStart("tcp", "release.invalid:443")
	case "read":
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

func TestDownloadRetriesPartialArchiveFromTheBeginning(t *testing.T) {
	binary := []byte("mesh-test-binary")
	archive := testArchive(t, binary)
	manifest := testManifestForArchive(binary, archive)
	var calls atomic.Int32
	transport := downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
		body := io.NopCloser(bytes.NewReader(archive))
		if calls.Add(1) == 1 {
			body = io.NopCloser(io.MultiReader(bytes.NewReader(archive[:len(archive)/2]), interruptedDownload{}))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: body, Request: request}, nil
	})
	client := Client{HTTPClient: &http.Client{Transport: transport}}
	cache := t.TempDir()
	path, err := client.Download(context.Background(), manifest, Platform{OS: "linux", Arch: "amd64"}, cache)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // Download returned a path inside the test cache
	if err != nil || !bytes.Equal(contents, binary) || calls.Load() != 2 {
		t.Fatalf("download = %q, %v, calls %d; want intact binary after two requests", contents, err, calls.Load())
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".archive-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary archives = %v, %v", leftovers, err)
	}
}

func TestDownloadRetriesTransientStatusOnly(t *testing.T) {
	contents, _ := json.Marshal(testManifest())
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
				code := http.StatusOK
				if calls.Add(1) == 1 {
					code = status
				}
				return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
			})}}
			_, err := client.Manifest(context.Background(), "v0.2.0")
			if status == http.StatusNotFound || status == http.StatusForbidden {
				if err == nil || calls.Load() != 1 {
					t.Fatalf("terminal status: error %v, calls %d; want one failure", err, calls.Load())
				}
				return
			}
			if err != nil || calls.Load() != 2 {
				t.Fatalf("transient status: error %v, calls %d; want recovery", err, calls.Load())
			}
		})
	}
}

func TestDownloadCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Request: request}, nil
	})}}
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	_, err := client.Manifest(ctx, "v0.2.0")
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 || time.Since(started) > time.Second {
		t.Fatalf("cancellation: error %v, calls %d, elapsed %s", err, calls.Load(), time.Since(started))
	}
}

type downloadRoundTripper func(*http.Request) (*http.Response, error)

func (transport downloadRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type interruptedDownload struct{}

func (interruptedDownload) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
