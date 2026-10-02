package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"testing/synctest"
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
	for _, stage := range []string{"DNS", "connect", "request write", "read"} {
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
	case "request write":
		trace.GotConn(httptrace.GotConnInfo{})
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: context.DeadlineExceeded})
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
	var timer *time.Timer
	client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		timer = time.AfterFunc(25*time.Millisecond, cancel)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Request: request}, nil
	})}}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
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

func TestLatestAPITimeoutLeavesFallbackBudget(t *testing.T) {
	contents, _ := json.Marshal(testManifest())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var fallback atomic.Bool
	client := Client{BaseURL: "https://release.invalid", LatestAPI: "https://release.invalid/api/latest", HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/latest" {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		fallback.Store(true)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
	})}}
	manifest, err := client.Manifest(ctx, "latest")
	if err != nil || manifest.Version != "v0.2.0" || !fallback.Load() {
		t.Fatalf("API timeout blocked fallback: version %q, error %v, fallback %t", manifest.Version, err, fallback.Load())
	}
}

func TestDownloadRetriesHTTP2BodyReset(t *testing.T) {
	binary := []byte("mesh-test-binary")
	archive := testArchive(t, binary)
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write(archive[:len(archive)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(archive)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	path, err := client.Download(context.Background(), testManifestForArchive(binary, archive), Platform{OS: "linux", Arch: "amd64"}, t.TempDir())
	if err != nil {
		t.Fatalf("HTTP/2 reset was not recovered: %v", err)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // Download returned a path inside the test cache
	if err != nil || !bytes.Equal(contents, binary) || calls.Load() != 2 {
		t.Fatalf("HTTP/2 download = %q, %v, calls %d; want intact binary after two requests", contents, err, calls.Load())
	}
}

func TestDownloadRejectsInsecureRedirectHop(t *testing.T) {
	var finalURL atomic.Value
	var insecureCalls atomic.Int32
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insecureCalls.Add(1)
		http.Redirect(w, r, finalURL.Load().(string), http.StatusFound)
	}))
	t.Cleanup(insecure.Close)
	contents, _ := json.Marshal(testManifest())
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			_, _ = w.Write(contents)
			return
		}
		http.Redirect(w, r, insecure.URL, http.StatusFound)
	}))
	finalURL.Store(secure.URL + "/final")
	t.Cleanup(secure.Close)
	client := Client{BaseURL: secure.URL, HTTPClient: secure.Client()}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	if err == nil || insecureCalls.Load() != 0 {
		t.Fatalf("insecure redirect: error %v, requests %d; want rejection before sending HTTP", err, insecureCalls.Load())
	}
}

func TestDownloadDestinationTimeoutStaysTerminal(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("archive")), Request: request}, nil
	})}
	err := retryDownload(context.Background(), assetDownloadTimeout, downloadAttemptTimeout, func(ctx context.Context) error {
		return writeDownload(ctx, client, "https://release.invalid/archive", failedDownloadDestination{}, "", maximumArchive)
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("destination error: %v, calls %d; want one terminal write failure", err, calls.Load())
	}
}

type failedDownloadDestination struct{}

func (failedDownloadDestination) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "test-archive", Err: context.DeadlineExceeded}
}

func TestManifestRetryAllowsSlowDNS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		contents, _ := json.Marshal(testManifest())
		var calls atomic.Int32
		client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return nil, context.DeadlineExceeded
			}
			deadline, ok := request.Context().Deadline()
			lookupDuration := 20090 * time.Millisecond
			if !ok || time.Until(deadline) < lookupDuration {
				return nil, fmt.Errorf("retry leaves less than the observed 20.09-second DNS lookup")
			}
			trace := httptrace.ContextClientTrace(request.Context())
			trace.DNSStart(httptrace.DNSStartInfo{Host: request.URL.Host})
			timer := time.NewTimer(lookupDuration)
			defer timer.Stop()
			select {
			case <-request.Context().Done():
				return nil, request.Context().Err()
			case <-timer.C:
				trace.DNSDone(httptrace.DNSDoneInfo{})
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
		})}}
		manifest, err := client.Manifest(context.Background(), "v0.2.0")
		if err != nil || manifest.Version != "v0.2.0" || calls.Load() != 2 {
			t.Fatalf("slow DNS retry: manifest %q, error %v, calls %d", manifest.Version, err, calls.Load())
		}
	})
}

func TestManifestRetriesHTTP2ResetBeforeHeaders(t *testing.T) {
	contents, _ := json.Marshal(testManifest())
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if calls.Add(1) == 1 {
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(contents)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	manifest, err := client.Manifest(context.Background(), "v0.2.0")
	if err != nil || manifest.Version != "v0.2.0" || calls.Load() != 2 {
		t.Fatalf("HTTP/2 reset before headers: version %q, error %v, requests %d", manifest.Version, err, calls.Load())
	}
}

func TestDownloadRejectsRedirectCallbackDowngrade(t *testing.T) {
	var insecureCalls atomic.Int32
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		insecureCalls.Add(1)
		_, _ = io.WriteString(w, "metadata")
	}))
	t.Cleanup(insecure.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	t.Cleanup(secure.Close)
	httpClient := secure.Client()
	httpClient.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		request.URL.Scheme = "http"
		request.URL.Host = insecure.Listener.Addr().String()
		return nil
	}
	client := Client{BaseURL: secure.URL, HTTPClient: httpClient}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	if err == nil || insecureCalls.Load() != 0 {
		t.Fatalf("redirect callback downgrade: error %v, insecure requests %d", err, insecureCalls.Load())
	}
}

func TestDownloadTotalBudgetCapsLaterAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		attempts := 0
		err := retryDownload(ctx, 350*time.Millisecond, 10*time.Millisecond, func(ctx context.Context) error {
			attempts++
			deadline, ok := ctx.Deadline()
			if attempts > 1 && (!ok || time.Until(deadline) > 150*time.Millisecond) {
				return fmt.Errorf("later attempt allowance exceeds the total download budget")
			}
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 2 || time.Since(started) > time.Second {
			t.Fatalf("total budget: error %v, attempts %d, elapsed %s; want deadline within 350ms budget including backoff", err, attempts, time.Since(started))
		}
	})
}

func TestManifestRetrySharesOverallBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		contents, _ := json.Marshal(testManifest())
		calls := 0
		started := time.Now()
		client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls < 3 {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			deadline, ok := request.Context().Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				return nil, fmt.Errorf("final attempt exceeds the shared metadata budget")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
		})}}
		manifest, err := client.Manifest(context.Background(), "v0.2.0")
		if err != nil || manifest.Version != "v0.2.0" || calls != 3 || time.Since(started) != 35750*time.Millisecond {
			t.Fatalf("shared metadata budget: manifest %q, error %v, calls %d, elapsed %s", manifest.Version, err, calls, time.Since(started))
		}
	})
}

func TestAssetDownloadSharesOverallBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		binary := []byte("mesh-test-binary")
		archive := testArchive(t, binary)
		manifest := testManifestForArchive(binary, archive)
		calls := 0
		started := time.Now()
		client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			<-request.Context().Done()
			return nil, request.Context().Err()
		})}}
		_, err := client.Download(context.Background(), manifest, Platform{OS: "linux", Arch: "amd64"}, t.TempDir())
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(started) != time.Minute {
			t.Fatalf("shared asset budget: error %v, calls %d, elapsed %s; want exhaustion at one minute", err, calls, time.Since(started))
		}
	})
}

func TestSecureClientKeepsRedirectPolicy(t *testing.T) {
	blocked := errors.New("custom redirect policy blocked the request")
	for _, tc := range []struct {
		name          string
		callbackError error
		lastResponse  bool
	}{
		{name: "last response", callbackError: http.ErrUseLastResponse, lastResponse: true},
		{name: "custom rejection", callbackError: blocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, "/final", http.StatusFound)
			}))
			t.Cleanup(server.Close)
			httpClient := server.Client()
			httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return tc.callbackError }
			client := Client{BaseURL: server.URL, HTTPClient: httpClient}
			_, err := client.Manifest(context.Background(), "v0.2.0")
			expected := errors.Is(err, tc.callbackError)
			if tc.lastResponse {
				var status *downloadStatusError
				expected = errors.As(err, &status) && status.code == http.StatusFound
			}
			if calls.Load() != 1 || !expected {
				t.Fatalf("redirect policy: error %v, requests %d; want one preserved policy result", err, calls.Load())
			}
		})
	}
}
