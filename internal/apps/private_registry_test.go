package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegistryIgnoresLegacyPublicStateAfterRestart(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	data, err := f.edgeStore.LoadAppState(context.Background(), "apps.edge")
	if err != nil {
		t.Fatal(err)
	}
	// A released registry stored this field both in records and cached results.
	data = bytes.ReplaceAll(data, []byte(`"kind":"static"`), []byte(`"kind":"static","visibility":"public"`))
	if err := f.edgeStore.SaveAppState(context.Background(), "apps.edge", data); err != nil {
		t.Fatal(err)
	}
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
			t.Fatalf("legacy public state granted %s: %d", path, response.Code)
		}
	}
	record, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || !record.ExpiresAt.Equal(app.ExpiresAt) || forwarded.Load() != 0 {
		t.Fatal("unauthorized legacy visits touched lifetime or source")
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
	data, err = f.edgeStore.LoadAppState(context.Background(), "apps.edge")
	if err != nil || bytes.Contains(data, []byte(`"visibility"`)) {
		t.Fatal("registry persisted retired visibility")
	}
	f.seq = f.edge.state.Owners[identityFor(f.ownerKey)].Sequence
	f.operation(t, Request{Action: "delete", ID: app.ID})
	if !f.edge.HasHost(app.ID + "." + Domain()) {
		t.Fatal("retired private hostname became available for unrelated routing")
	}
}

func TestRemovedSharingActionsRejectCachedSuccessAndHTTP(t *testing.T) {
	for _, action := range []string{"public", "private"} {
		t.Run(action, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			owner := pairedOwner(t, f)
			f.seq = f.origin.state.Sequence
			request := f.signed(t, Request{Action: action, ID: app.ID})
			f.edge.state.Owners[request.Owner] = ownerState{Sequence: request.Sequence, ID: request.ID, Digest: digestBytes(request.Body), Result: Result{App: &app}}
			if err := save(context.Background(), f.edgeStore, "apps.edge", f.edge.state); err != nil {
				t.Fatal(err)
			}
			f.edge = f.openEdge(t)
			f.origin.config.Exchange = f.edge.Exchange
			before := registryStateBytes(t, f.edge)
			if _, err := f.exchange(t, request); err == nil || !strings.Contains(err.Error(), "unsupported operation") {
				t.Fatalf("retired cached action accepted: %v", err)
			}
			if _, err := f.origin.Handle(context.Background(), Request{Action: action, ID: app.ID}); err == nil {
				t.Fatal("origin accepted retired action")
			}
			auth := httptest.NewRequest(http.MethodGet, ManagementOrigin(), nil)
			auth.AddCookie(owner)
			session, err := f.edge.auth.Browser(context.Background(), auth)
			if err != nil {
				t.Fatal(err)
			}
			form := url.Values{"id": {app.ID}, "action": {action}, "csrf": {session.CSRF}, "confirmation": {app.ID}}
			post := httptest.NewRequest(http.MethodPost, ManagementOrigin()+"/action", strings.NewReader(form.Encode()))
			post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			post.Header.Set("Origin", ManagementOrigin())
			post.AddCookie(owner)
			response := httptest.NewRecorder()
			f.edge.ServeHost(response, post, ManagementHost())
			if response.Code != http.StatusBadRequest {
				t.Fatalf("retired HTTP action status=%d", response.Code)
			}
			for _, path := range []string{"/confirm?id=" + app.ID + "&action=" + action, "/frame?id=" + app.ID} {
				response = httptest.NewRecorder()
				get := httptest.NewRequest(http.MethodGet, ManagementOrigin()+path, nil)
				get.AddCookie(owner)
				f.edge.ServeHost(response, get, ManagementHost())
				if response.Code != http.StatusNotFound {
					t.Fatalf("retired control %s status=%d", path, response.Code)
				}
			}
			if !reflect.DeepEqual(before, registryStateBytes(t, f.edge)) {
				t.Fatal("removed operations mutated registry")
			}
			var decoded Request
			if err := decode([]byte(`{"action":"renew","visibility":"public"}`), &decoded); err == nil {
				t.Fatal("removed visibility request field accepted")
			}
		})
	}
}

func TestPendingSharingActionSettlesWithoutBlockingOwnerLifecycle(t *testing.T) {
	for _, action := range []string{"public", "private"} {
		t.Run(action, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			f.origin.state.Sequence++
			pending, err := Sign("mesh-app/request/v1", identityFor(f.edgeKey), f.origin.state.Sequence, map[string]string{"action": action, "id": app.ID, "visibility": action}, f.ownerKey, f.now)
			if err != nil {
				t.Fatal(err)
			}
			f.origin.state.Pending = &pending
			if err := f.origin.persist(context.Background()); err != nil {
				t.Fatal(err)
			}
			recovered, err := NewOrigin(context.Background(), f.origin.config)
			if err != nil {
				t.Fatal(err)
			}
			f.origin = recovered
			if _, err := f.origin.Handle(context.Background(), Request{Action: "inspect", ID: app.ID}); err == nil || !strings.Contains(err.Error(), "unsupported operation") {
				t.Fatalf("retired pending action error=%v", err)
			}
			if f.origin.state.Pending != nil {
				t.Fatal("retired action left owner queue blocked")
			}
			var durable originState
			if err := load(context.Background(), f.originStore, "apps.origin", &durable); err != nil || durable.Pending != nil {
				t.Fatal("retired action acknowledgement was not durable")
			}
			result, err := f.origin.Handle(context.Background(), Request{Action: "inspect", ID: app.ID})
			if err != nil || result.App == nil || result.App.ID != app.ID {
				t.Fatalf("owner operation remained blocked: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(f.origin.state.Apps[app.ID].Root, "index.html"))
			if err != nil || string(data) != "original page" {
				t.Fatal("settling retired action changed source")
			}
			encoded, err := json.Marshal(result)
			if err != nil || bytes.Contains(encoded, []byte(`"visibility"`)) {
				t.Fatal("owner result exposes retired visibility")
			}
		})
	}
}
