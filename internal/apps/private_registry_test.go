package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryPreservesPrivateStateAcrossRestart(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	f.edge = f.openEdge(t)
	f.origin.config.Exchange = f.edge.Exchange
	forwarded := networkOrigin(t, f)
	f.now = f.now.Add(time.Hour)
	for _, path := range []string{"/", "/index.html", "/socket"} {
		request := httptest.NewRequest(http.MethodGet, URL(app.ID)+path, nil)
		if path == "/socket" {
			request.Header.Set("Upgrade", "websocket")
		}
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, request, app.ID+"."+Domain())
		if response.Code < 300 || strings.Contains(response.Body.String(), "original page") {
			t.Fatalf("private state granted %s: %d", path, response.Code)
		}
	}
	record, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || !record.ExpiresAt.Equal(app.ExpiresAt) || forwarded.Load() != 0 {
		t.Fatal("unauthorized visits touched lifetime or source")
	}
	for _, domain := range []string{Domain(), "old.test"} {
		if !f.edge.HasHost(app.ID+"."+domain) || !f.edge.HasHost("apps."+domain) {
			t.Fatalf("private host missing for %s", domain)
		}
	}
	if f.edge.HasHost(app.ID + ".other.test") {
		t.Fatal("unconfigured private host accepted")
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, ownerRequest(f, httptest.NewRequest(http.MethodGet, URL(app.ID), nil)), app.ID+"."+Domain())
	if response.Code != http.StatusOK || response.Body.String() != "original page" {
		t.Fatalf("owner lost preserved app: %d %s", response.Code, response.Body.String())
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "renew", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	data, err := f.edgeStore.LoadAppState(context.Background(), "apps.edge")
	if err != nil || len(data) == 0 {
		t.Fatal("registry lost durable private state")
	}
	f.seq = f.edge.state.Owners[identityFor(f.ownerKey)].Sequence
	f.operation(t, Request{Action: "delete", ID: app.ID})
	if !f.edge.HasHost(app.ID + "." + Domain()) {
		t.Fatal("retired private hostname became available for unrelated routing")
	}
}
