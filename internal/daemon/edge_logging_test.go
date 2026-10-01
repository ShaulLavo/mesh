package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/transport"
)

type edgeLogCapture struct {
	mu       sync.Mutex
	records  []string
	received chan struct{}
}

func (c *edgeLogCapture) append(err error) {
	c.mu.Lock()
	c.records = append(c.records, err.Error())
	c.mu.Unlock()
	select {
	case c.received <- struct{}{}:
	default:
	}
}
func (c *edgeLogCapture) contains(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, record := range c.records {
		if strings.Contains(record, text) {
			return true
		}
	}
	return false
}
func (c *edgeLogCapture) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.records) }

type edgeSinkObserver struct {
	writer io.Writer
	origin chan struct{}
	once   sync.Once
}

func (w *edgeSinkObserver) Write(raw []byte) (int, error) {
	if strings.Contains(string(raw), "event=origin-unavailable") {
		w.once.Do(func() { close(w.origin) })
	}
	n, err := w.writer.Write(raw)
	if err != nil {
		return n, errors.Join(errors.New("observe edge sink"), err)
	}
	return n, nil
}

func TestProductionEdgeSinkPreservesCategoryBehindBlockedOperator(t *testing.T) {
	capture := &edgeLogCapture{received: make(chan struct{}, 256)}
	entered := make(chan struct{})
	release := make(chan struct{})
	var first, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	reporter := newErrorReporter(func(err error) { first.Do(func() { close(entered); <-release }); capture.append(err) })
	defer reporter.shutdown()
	observer := &edgeSinkObserver{writer: edgeReportWriter{reporter: reporter}, origin: make(chan struct{})}
	var clockMu sync.Mutex
	now := time.Now()
	registry, err := edge.NewRegistry(edge.HandlerConfig{Mode: edge.ModeDirectTLS, Logger: log.New(observer, "", 0), Now: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }, DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("fixture origin unavailable")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	invalid := func() {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "invalid.example"
		registry.ServeHTTP(httptest.NewRecorder(), r)
	}
	invalid()
	select {
	case <-entered:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("operator was not reached")
	}
	for range 59 {
		invalid()
	}
	clockMu.Lock()
	now = now.Add(time.Minute)
	clockMu.Unlock()
	for range 60 {
		invalid()
	}
	origin, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Replace([]edge.PublishedRoute{{Route: edge.Route{PublicName: "docs.shaulavo.dev", ServiceName: "docs"}, Origin: edge.ResolvedOrigin{Identity: origin.ID, DisplayAlias: "fixture", Endpoint: netip.MustParseAddrPort("127.0.0.1:9"), Online: true, OnlineUntil: now.Add(time.Minute), LastSeenAt: now}}}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/docs/", nil)
	request.Host = "docs.shaulavo.dev"
	request.RemoteAddr = "192.0.2.1:1234"
	request.TLS = &tls.ConnectionState{ServerName: request.Host}
	registry.ServeHTTP(httptest.NewRecorder(), request)
	// The old bridge reaches the origin record while the operator is blocked;
	// the fixed bridge leaves that record in the authoritative category queue.
	select {
	case <-observer.origin:
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	registry.Close()
	deadline := time.After(runtimeTestTimeout)
	for !capture.contains("event=origin-unavailable") {
		select {
		case <-capture.received:
		case <-deadline:
			t.Fatal("shared reporter silently lost the origin failure")
		}
	}
	if !capture.contains("event=events-dropped category=invalid-public-host dropped=55") {
		t.Fatal("production sink lost the exact category overflow count")
	}
}

func TestDaemonTeardownFlushesCurrentEdgeDropSummary(t *testing.T) {
	capture := &edgeLogCapture{received: make(chan struct{}, 128)}
	reporter := newErrorReporter(capture.append)
	registry, err := edge.NewRegistry(edge.HandlerConfig{Mode: edge.ModeDirectTLS, Logger: log.New(edgeReportWriter{reporter: reporter}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for range 61 {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "invalid.example"
		registry.ServeHTTP(httptest.NewRecorder(), r)
	}
	deadline := time.After(runtimeTestTimeout)
	for capture.count() < 60 {
		select {
		case <-capture.received:
		case <-deadline:
			t.Fatal("admitted records were not delivered")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = serveListeners(ctx, cancel, listenerConfig{reporter: reporter, publicHTTPHandler: registry}, func(context.Context, transport.Conn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !capture.contains("event=events-dropped category=invalid-public-host dropped=1") {
		t.Fatal("daemon returned before its final edge summary reached the operator")
	}
}
