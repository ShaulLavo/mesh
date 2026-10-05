package apps_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/storage"
)

func TestStaticAssetBurstThroughSharedRegistry(t *testing.T) {
	t.Run("app", func(t *testing.T) { checkStaticAssetBurst(t, false) })
	t.Run("service", func(t *testing.T) { checkStaticAssetBurst(t, true) })
}

func checkStaticAssetBurst(t *testing.T, service bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	registry, err := edge.NewRegistry(edge.HandlerConfig{Mode: edge.ModeDirectTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	edgePublic, edgeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerPublic, ownerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for index := range 10 {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("image-%d.svg", index)), []byte("<svg/>"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan struct{}, 10)
	finish := make(chan struct{}, 10)
	var active, peak atomic.Int64
	files := http.FileServer(http.Dir(source))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		parts := strings.SplitN(r.URL.Path, "/", 5)
		if len(parts) == 5 {
			r.URL.Path = "/" + parts[4]
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets")
		files.ServeHTTP(w, r)
	}))
	defer origin.Close()
	defer close(finish)
	endpoint, err := netip.ParseAddrPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	appEdge, err := apps.NewEdge(ctx, apps.EdgeConfig{
		Store: store, Key: edgeKey, Allowed: map[string]bool{base64.RawURLEncoding.EncodeToString(ownerPublic): true},
		Acquire: registry.AcquireApp, ClientIP: registry.AppClientIP,
		Resolve: func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer appEdge.Close()
	registry.SetAppHandler(appEdge)
	var sequence uint64
	operation := func(request apps.Request) apps.Result {
		t.Helper()
		sequence++
		signed, err := apps.Sign("mesh-app/request/v1", base64.RawURLEncoding.EncodeToString(edgePublic), sequence, request, ownerKey, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		response, err := appEdge.Exchange(ctx, signed)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Result apps.Result
			Error  string
		}
		if err := json.Unmarshal(response.Body, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error != "" {
			t.Fatal(reply.Error)
		}
		return reply.Result
	}
	app := operation(apps.Request{Action: "allocate", Kind: "static"}).App
	operation(apps.Request{Action: "activate", ID: app.ID})
	operation(apps.Request{Action: "public", ID: app.ID})
	responses := make(chan *httptest.ResponseRecorder, 10)
	var requests sync.WaitGroup
	launch := func(index int, host, prefix string) {
		requests.Add(1)
		go func() {
			defer requests.Done()
			request := httptest.NewRequest(http.MethodGet, prefix+fmt.Sprintf("/image-%d.svg", index), nil).WithContext(ctx)
			request.Host = host
			request.RemoteAddr = "198.51.100.1:12345"
			request.TLS = &tls.ConnectionState{ServerName: host}
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			responses <- response
		}()
	}
	defer func() { cancel(); requests.Wait() }()
	initialHost := app.ID + "." + apps.Domain
	initialPrefix := ""
	if service {
		initialHost = "service.shaulavo.dev"
		initialPrefix = "/assets"
		now := time.Now()
		err := registry.Replace([]edge.PublishedRoute{{
			Route:  edge.Route{PublicName: initialHost, ServiceName: "assets"},
			Origin: edge.ResolvedOrigin{Identity: base64.RawURLEncoding.EncodeToString(ownerPublic), DisplayAlias: "Static assets", Endpoint: endpoint, SnapshotSequence: 1, Online: true, OnlineUntil: now.Add(time.Minute), LastSeenAt: now},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := range 8 {
		launch(index, initialHost, initialPrefix)
	}
	for range 8 {
		select {
		case <-entered:
		case response := <-responses:
			t.Fatalf("first asset failed: %d %s", response.Code, response.Body.String())
		case <-time.After(5 * time.Second):
			t.Fatal("first eight assets did not reach origin")
		}
	}
	launch(8, app.ID+"."+apps.Domain, "")
	launch(9, app.ID+"."+apps.Domain, "")
	select {
	case response := <-responses:
		t.Fatalf("normal asset burst rejected: %d %s", response.Code, response.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	// Permit held and queued responses through the real upstream HTTP server.
	for range 10 {
		finish <- struct{}{}
	}
	for range 10 {
		response := receiveBurstResponse(t, responses)
		if response.Code != http.StatusOK || response.Body.String() != "<svg/>" {
			t.Fatalf("asset: %d %s", response.Code, response.Body.String())
		}
	}

	if peak.Load() != 8 || active.Load() != 0 {
		t.Fatalf("active=%d peak=%d", active.Load(), peak.Load())
	}
}

func receiveBurstResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("asset queue did not drain")
	}
	return nil
}
