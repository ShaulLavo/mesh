package release

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type goAwayConnectionKey struct{}

type goAwayFixture struct {
	server      *httptest.Server
	calls       atomic.Int32
	posts       atomic.Int32
	connections atomic.Int32
	disconnect  atomic.Bool
	alwaysFail  atomic.Bool
}

func newGoAwayFixture(t *testing.T, contents []byte, redirect bool) *goAwayFixture {
	t.Helper()
	fixture := &goAwayFixture{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if redirect && r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		fixture.calls.Add(1)
		if r.Method == http.MethodPost {
			fixture.posts.Add(1)
		}
		if fixture.disconnect.Swap(false) || fixture.alwaysFail.Load() {
			conn := r.Context().Value(goAwayConnectionKey{}).(net.Conn)
			framer := http2.NewFramer(conn, conn)
			if err := framer.WriteGoAway(0x7fffffff, http2.ErrCodeNo, nil); err != nil {
				t.Errorf("write accepted-stream GOAWAY: %v", err)
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write(contents)
	}))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		fixture.connections.Add(1)
		return context.WithValue(ctx, goAwayConnectionKey{}, conn)
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	fixture.server = server
	return fixture
}

func TestManifestRecoversAcceptedHTTP2GoAwayBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redirect bool
	}{{name: "direct"}, {name: "redirected", redirect: true}} {
		t.Run(tc.name, func(t *testing.T) {
			contents, err := json.Marshal(testManifest())
			if err != nil {
				t.Fatal(err)
			}
			fixture := newGoAwayFixture(t, contents, tc.redirect)
			client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
			if manifest, err := client.Manifest(context.Background(), "v0.2.0"); err != nil || manifest.Version != "v0.2.0" || fixture.calls.Load() != 1 {
				t.Fatalf("known-good HTTP/2 manifest: version %q, error %v, requests %d", manifest.Version, err, fixture.calls.Load())
			}
			fixture.disconnect.Store(true)
			manifest, err := client.Manifest(context.Background(), "v0.2.0")
			if err != nil || manifest.Version != "v0.2.0" || fixture.calls.Load() != 3 || fixture.connections.Load() != 2 {
				t.Fatalf("accepted-stream GOAWAY not recovered: version %q, error %v, requests %d, connections %d; want one fresh-connection retry", manifest.Version, err, fixture.calls.Load(), fixture.connections.Load())
			}
		})
	}
}

func TestHTTP2GoAwayUnknownRetryIsBounded(t *testing.T) {
	contents, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newGoAwayFixture(t, contents, false)
	client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
	if _, err := client.Manifest(context.Background(), "v0.2.0"); err != nil {
		t.Fatalf("known-good HTTP/2 manifest: %v", err)
	}
	fixture.alwaysFail.Store(true)
	_, err = client.Manifest(context.Background(), "v0.2.0")
	if err == nil || fixture.calls.Load() != 3 || !strings.Contains(err.Error(), "attempt 2/3") {
		t.Fatalf("opaque HTTP/2 failure escaped one-retry cap: %v, requests %d", err, fixture.calls.Load())
	}
	fixture.alwaysFail.Store(false)
	if _, err := client.Manifest(context.Background(), "v0.2.0"); err != nil || fixture.calls.Load() != 4 {
		t.Fatalf("fresh connection did not recover after bounded failure: %v, requests %d", err, fixture.calls.Load())
	}
}

func TestArchiveRecoversAcceptedHTTP2GoAwayBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redirect bool
	}{{name: "direct"}, {name: "redirected", redirect: true}} {
		t.Run(tc.name, func(t *testing.T) {
			binary := []byte("mesh-test-binary")
			archive := testArchive(t, binary)
			fixture := newGoAwayFixture(t, archive, tc.redirect)
			client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
			manifest := testManifestForArchive(binary, archive)
			platform := Platform{OS: "linux", Arch: "amd64"}
			if _, err := client.Download(context.Background(), manifest, platform, t.TempDir()); err != nil {
				t.Fatalf("known-good HTTP/2 archive: %v", err)
			}
			fixture.disconnect.Store(true)
			path, err := client.Download(context.Background(), manifest, platform, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path) //nolint:gosec // Download returned a path inside the test cache
			if err != nil || !bytes.Equal(contents, binary) || fixture.calls.Load() != 3 || fixture.connections.Load() != 2 {
				t.Fatalf("GOAWAY archive recovery: binary %q, error %v, requests %d, connections %d", contents, err, fixture.calls.Load(), fixture.connections.Load())
			}
		})
	}
}

func TestHTTP2UnknownRetryKeepsPermanentErrorsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "certificate", err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
		{name: "TLS record", err: tls.RecordHeaderError{}},
		{name: "TLS alert", err: tls.AlertError(42)},
		{name: "unknown authority", err: x509.UnknownAuthorityError{}},
		{name: "hostname", err: x509.HostnameError{}},
		{name: "invalid certificate", err: x509.CertificateInvalidError{}},
		{name: "system roots", err: x509.SystemRootsError{}},
		{name: "DNS not found", err: &net.DNSError{IsNotFound: true}},
		{name: "stream protocol", err: http2.StreamError{Code: http2.ErrCodeProtocol}},
		{name: "cancelled", err: context.Canceled},
		{name: "redirect timeout", err: &downloadRedirectError{err: context.DeadlineExceeded}},
		{name: "destination timeout", err: &os.PathError{Op: "write", Path: "fixture", Err: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &downloadTransportError{err: tc.err, awaitingHTTP2Headers: true}
			if policy := downloadRetryPolicy(err); policy != downloadTerminal {
				t.Fatalf("permanent failure got policy %d, want terminal", policy)
			}
		})
	}
}

func TestHTTP2RedirectTimeoutIsTerminal(t *testing.T) {
	fixture := newGoAwayFixture(t, nil, true)
	httpClient := fixture.server.Client()
	var redirects atomic.Int32
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		redirects.Add(1)
		return context.DeadlineExceeded
	}
	client := Client{BaseURL: fixture.server.URL, HTTPClient: httpClient}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	if !errors.Is(err, context.DeadlineExceeded) || redirects.Load() != 1 || fixture.calls.Load() != 0 {
		t.Fatalf("permanent redirect callback failure retried: %v, callbacks %d, final requests %d", err, redirects.Load(), fixture.calls.Load())
	}
}

func TestHTTP2GoAwayCancellationStopsUnknownBackoff(t *testing.T) {
	contents, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newGoAwayFixture(t, contents, false)
	client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
	if _, err := client.Manifest(context.Background(), "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	fixture.disconnect.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = client.Manifest(ctx, "v0.2.0")
	if !errors.Is(err, context.DeadlineExceeded) || fixture.calls.Load() != 2 {
		t.Fatalf("unknown backoff ignored cancellation: %v, requests %d", err, fixture.calls.Load())
	}
}

func TestUnknownTransportWithoutHTTP2TraceIsTerminal(t *testing.T) {
	var calls atomic.Int32
	blocked := errors.New("fixture transport rejection")
	client := Client{HTTPClient: &http.Client{Transport: downloadRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, blocked
	})}}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	if !errors.Is(err, blocked) || calls.Load() != 1 {
		t.Fatalf("untraced supplied transport got unknown retry: %v, requests %d", err, calls.Load())
	}
}

func TestHTTP2ValidationErrorsStayTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents []byte
	}{
		{name: "manifest JSON", contents: []byte("{")},
		{name: "manifest size", contents: bytes.Repeat([]byte("x"), maximumManifest+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGoAwayFixture(t, tc.contents, false)
			client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
			_, err := client.Manifest(context.Background(), "v0.2.0")
			if err == nil || fixture.calls.Load() != 1 {
				t.Fatalf("invalid manifest retried: %v, requests %d", err, fixture.calls.Load())
			}
		})
	}
	binary := []byte("mesh-test-binary")
	archive := testArchive(t, binary)
	fixture := newGoAwayFixture(t, archive, false)
	client := Client{BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
	manifest := testManifestForArchive(binary, archive)
	for i := range manifest.Artifacts {
		manifest.Artifacts[i].SHA256 = testDigest([]byte("wrong archive"))
	}
	_, err := client.Download(context.Background(), manifest, Platform{OS: "linux", Arch: "amd64"}, t.TempDir())
	if err == nil || fixture.calls.Load() != 1 {
		t.Fatalf("invalid archive digest retried: %v, requests %d", err, fixture.calls.Load())
	}
}

func TestHTTP2CertificateFailureStaysTerminal(t *testing.T) {
	fixture := newGoAwayFixture(t, nil, false)
	transport := &http.Transport{ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := Client{BaseURL: fixture.server.URL, HTTPClient: &http.Client{Transport: transport}}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	var certificate *tls.CertificateVerificationError
	if !errors.As(err, &certificate) || fixture.connections.Load() != 1 || fixture.calls.Load() != 0 {
		t.Fatalf("untrusted TLS fixture retried: %v, connections %d, requests %d", err, fixture.connections.Load(), fixture.calls.Load())
	}
}

func TestPermanentDNSFailureStaysTerminal(t *testing.T) {
	var calls atomic.Int32
	transport := &http.Transport{ForceAttemptHTTP2: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "fixture.invalid", IsNotFound: true}}
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := Client{BaseURL: "https://fixture.invalid", HTTPClient: &http.Client{Transport: transport}}
	_, err := client.Manifest(context.Background(), "v0.2.0")
	var dns *net.DNSError
	if !errors.As(err, &dns) || calls.Load() != 1 {
		t.Fatalf("permanent DNS failure retried: %v, dials %d", err, calls.Load())
	}
}

func TestHTTP2RedirectCallbackCannotChangeDownloadMethod(t *testing.T) {
	contents, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newGoAwayFixture(t, contents, true)
	httpClient := fixture.server.Client()
	client := Client{BaseURL: fixture.server.URL, HTTPClient: httpClient}
	if manifest, err := client.Manifest(context.Background(), "v0.2.0"); err != nil || manifest.Version != "v0.2.0" || fixture.calls.Load() != 1 || fixture.posts.Load() != 0 {
		t.Fatalf("known-good redirected HTTP/2 GET: version %q, error %v, requests %d, POSTs %d", manifest.Version, err, fixture.calls.Load(), fixture.posts.Load())
	}
	var redirects atomic.Int32
	httpClient.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		redirects.Add(1)
		request.Method = http.MethodPost
		return nil
	}
	fixture.disconnect.Store(true)
	_, err = client.Manifest(context.Background(), "v0.2.0")
	var policy *downloadRedirectError
	if !errors.As(err, &policy) || redirects.Load() != 1 || fixture.posts.Load() != 0 || fixture.calls.Load() != 1 {
		t.Fatalf("method-changing callback escaped terminal GET policy: error %v, callbacks %d, requests %d, POSTs %d", err, redirects.Load(), fixture.calls.Load(), fixture.posts.Load())
	}
}
