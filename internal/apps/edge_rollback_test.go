package apps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

var errEdgeSave = errors.New("edge disk full")

type failingEdgeStore struct {
	*memoryAppStore
	fail          bool
	attemptedName string
}

func (s *failingEdgeStore) SaveAppState(ctx context.Context, key string, value []byte) error {
	if key == "apps.edge" && s.fail {
		return errEdgeSave
	}
	return s.memoryAppStore.SaveAppState(ctx, key, value)
}

func (s *failingEdgeStore) ReserveAppNameAndState(ctx context.Context, name, owner, key string, value []byte) error {
	s.attemptedName = name
	if s.fail {
		return errEdgeSave
	}
	return s.memoryAppStore.ReserveAppNameAndState(ctx, name, owner, key, value)
}

func edgeStateBytes(t *testing.T, e *Edge) []byte {
	t.Helper()
	b, err := json.Marshal(e.state)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEdgeFailedAllocationLeavesNoPhantom(t *testing.T) {
	f := newAppFixture(t)
	store := &failingEdgeStore{memoryAppStore: f.edgeStore, fail: true}
	f.edge.config.Store = store
	before := edgeStateBytes(t, f.edge)
	request := f.signed(t, Request{Action: "allocate", Kind: "static"})
	if _, err := f.edge.Exchange(context.Background(), request); !errors.Is(err, errEdgeSave) {
		t.Fatalf("allocation error = %v, want failed save", err)
	}
	if after := edgeStateBytes(t, f.edge); !reflect.DeepEqual(before, after) {
		t.Errorf("failed allocation changed live state: apps=%d owners=%d", len(f.edge.state.Apps), len(f.edge.state.Owners))
	}
	if exists, err := store.AppNameExists(context.Background(), store.attemptedName); err != nil || exists {
		t.Errorf("failed allocation reserved a name: exists=%v error=%v", exists, err)
	}
	store.fail = false
	f.now = f.now.Add(12 * time.Hour)
	healthy := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	f.operation(t, Request{Action: "activate", ID: healthy.ID})
	f.operation(t, Request{Action: "public", ID: healthy.ID})
	f.now = f.now.Add(13 * time.Hour)
	if err := f.edge.Sweep(context.Background()); err != nil {
		t.Errorf("sweep after phantom deadline: %v", err)
	}
	if app, exists, err := f.edge.lookup(context.Background(), healthy.ID, false); err != nil || !exists || app.Status != "active" {
		t.Errorf("healthy lookup after phantom deadline: status=%s exists=%v error=%v", app.Status, exists, err)
	}
	_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(healthy.ID), nil), healthy.ID)
	if err != nil {
		t.Errorf("healthy admission after phantom deadline: %v", err)
	} else {
		release()
	}
	if _, err := f.exchange(t, f.signed(t, Request{Action: "inspect", ID: healthy.ID})); err != nil {
		t.Errorf("healthy owner operation after phantom deadline: %v", err)
	}
	if len(f.edge.state.Apps) != 1 {
		t.Errorf("edge has %d apps after failed allocation, want only the healthy app", len(f.edge.state.Apps))
	}
}

func TestEdgeSweepIsolatesUnfreeableApp(t *testing.T) {
	f := newAppFixture(t)
	bad := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	good := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	delete(f.edgeStore.names, bad.ID+"."+Domain)
	f.now = f.now.Add(12 * time.Hour)
	healthy := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	f.operation(t, Request{Action: "activate", ID: healthy.ID})
	f.operation(t, Request{Action: "public", ID: healthy.ID})
	f.now = f.now.Add(12 * time.Hour)
	err := f.edge.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), bad.ID) {
		t.Errorf("sweep must report the unfreeable app %s: %v", bad.ID, err)
	}
	for _, id := range []string{bad.ID, good.ID} {
		if app := f.edge.state.Apps[id]; app.Status != "expired" || app.Ready {
			t.Errorf("sweep did not expire app %s: %#v", id, app)
		}
	}
	if !f.edgeStore.inactive[good.ID+"."+Domain] {
		t.Error("sweep did not retire the other expired app's name")
	}
	if app, exists, err := f.edge.lookup(context.Background(), healthy.ID, false); err != nil || !exists || app.Status != "active" {
		t.Errorf("unfreeable app broke healthy lookup: status=%s exists=%v error=%v", app.Status, exists, err)
	}
	_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(healthy.ID), nil), healthy.ID)
	if err != nil {
		t.Errorf("unfreeable app broke healthy admission: %v", err)
	} else {
		release()
	}
	if _, err := f.exchange(t, f.signed(t, Request{Action: "inspect", ID: healthy.ID})); err != nil {
		t.Errorf("unfreeable app broke healthy owner operation: %v", err)
	}
}

func TestEdgeFailedMutationPreservesStateAndRequests(t *testing.T) {
	for _, surface := range []string{"exchange", "http"} {
		for _, action := range []string{"private", "delete", "renew"} {
			t.Run(surface+"/"+action, func(t *testing.T) {
				f := newAppFixture(t)
				app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
				f.operation(t, Request{Action: "activate", ID: app.ID})
				f.operation(t, Request{Action: "public", ID: app.ID})
				_, admitted, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				before := edgeStateBytes(t, f.edge)
				store := &failingEdgeStore{memoryAppStore: f.edgeStore, fail: true}
				f.edge.config.Store = store
				f.now = f.now.Add(time.Hour)
				if surface == "exchange" {
					if _, err := f.edge.Exchange(context.Background(), f.signed(t, Request{Action: action, ID: app.ID})); !errors.Is(err, errEdgeSave) {
						t.Fatalf("mutation error = %v, want failed save", err)
					}
				} else {
					form := url.Values{"id": {app.ID}, "action": {action}, "csrf": {"fixture-csrf"}}
					r := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(form.Encode()))
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					r.Header.Set("Origin", ManagementOrigin)
					r = r.WithContext(context.WithValue(r.Context(), networkSessionKey{}, webauth.Session{Owners: []string{app.Owner}, CSRF: "fixture-csrf"}))
					w := httptest.NewRecorder()
					f.edge.ServeHost(w, r, ManagementHost)
					if w.Code != http.StatusServiceUnavailable {
						t.Fatalf("mutation status = %d, want failed save", w.Code)
					}
				}
				if after := edgeStateBytes(t, f.edge); !reflect.DeepEqual(before, after) {
					t.Error("failed mutation changed live state")
				}
				if admitted.Context().Err() != nil {
					t.Error("failed mutation canceled a live request")
				}
				if f.edgeStore.inactive[app.ID+"."+Domain] {
					t.Error("failed mutation retired the app name")
				}
			})
		}
	}
}

func TestEdgeFailedAllocationCanRetrySameRequest(t *testing.T) {
	f := newAppFixture(t)
	store := &failingEdgeStore{memoryAppStore: f.edgeStore, fail: true}
	f.edge.config.Store = store
	request := f.signed(t, Request{Action: "allocate", Kind: "static"})
	if _, err := f.edge.Exchange(context.Background(), request); !errors.Is(err, errEdgeSave) {
		t.Fatalf("allocation error = %v, want failed save", err)
	}
	store.fail = false
	result, err := f.exchange(t, request)
	if err != nil || result.App == nil {
		t.Fatalf("retry failed: %#v %v", result, err)
	}
	if exists, err := store.AppNameExists(context.Background(), result.App.ID+"."+Domain); err != nil || !exists {
		t.Fatalf("retry acknowledged an unreserved app: exists=%v error=%v", exists, err)
	}
	f.edge = f.openEdge(t)
	replayed, err := f.exchange(t, request)
	if err != nil || !reflect.DeepEqual(result, replayed) || len(f.edge.state.Apps) != 1 {
		t.Fatalf("durable retry changed allocation: %#v %v", replayed, err)
	}
}

func TestEdgeFailedExpirySaveLeavesLiveState(t *testing.T) {
	for _, surface := range []string{"sweep", "lookup", "admission", "exchange"} {
		t.Run(surface, func(t *testing.T) {
			f := newAppFixture(t)
			app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
			f.operation(t, Request{Action: "activate", ID: app.ID})
			f.operation(t, Request{Action: "public", ID: app.ID})
			_, admitted, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			f.now = f.now.Add(time.Hour)
			healthy := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
			f.operation(t, Request{Action: "activate", ID: healthy.ID})
			f.operation(t, Request{Action: "public", ID: healthy.ID})
			f.now = app.ExpiresAt
			before := edgeStateBytes(t, f.edge)
			storedBefore, err := f.edgeStore.LoadAppState(context.Background(), "apps.edge")
			if err != nil {
				t.Fatal(err)
			}
			f.edge.config.Store = &failingEdgeStore{memoryAppStore: f.edgeStore, fail: true}
			switch surface {
			case "sweep":
				err = f.edge.Sweep(context.Background())
			case "lookup":
				_, _, err = f.edge.lookup(context.Background(), healthy.ID, false)
			case "admission":
				_, _, release, admitErr := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(healthy.ID), nil), healthy.ID)
				if release != nil {
					release()
				}
				err = admitErr
			case "exchange":
				_, err = f.edge.Exchange(context.Background(), f.signed(t, Request{Action: "inspect", ID: healthy.ID}))
			}
			if !errors.Is(err, errEdgeSave) {
				t.Errorf("expiry save error = %v, want failed save", err)
			}
			if after := edgeStateBytes(t, f.edge); !reflect.DeepEqual(before, after) {
				t.Error("failed expiry save changed live state")
			}
			storedAfter, err := f.edgeStore.LoadAppState(context.Background(), "apps.edge")
			if err != nil || !reflect.DeepEqual(storedBefore, storedAfter) {
				t.Errorf("failed expiry save changed durable state: %v", err)
			}
			if admitted.Context().Err() != nil || f.edgeStore.inactive[app.ID+"."+Domain] || len(f.edge.pendingRetire) != 0 {
				t.Error("failed expiry save canceled a request or retired a name")
			}
		})
	}
}

func TestEdgeNameRetirementRetriesAcrossRestartAndCleanup(t *testing.T) {
	f := newAppFixture(t)
	app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	delete(f.edgeStore.names, app.ID+"."+Domain)
	f.now = app.ExpiresAt
	if err := f.edge.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), app.ID) {
		t.Fatalf("sweep did not report failed name retirement: %v", err)
	}
	f.operation(t, Request{Action: "cleanup", ID: app.ID})
	f.edge = f.openEdge(t)
	if err := f.edgeStore.ReserveAppName(context.Background(), app.ID+"."+Domain, app.Owner); err != nil {
		t.Fatal(err)
	}
	if err := f.edge.Sweep(context.Background()); err != nil {
		t.Fatalf("retirement did not recover: %v", err)
	}
	if !f.edgeStore.inactive[app.ID+"."+Domain] || len(f.edge.pendingRetire) != 0 {
		t.Fatal("recovered name retirement remained pending")
	}
}

func TestEdgeAllocationRejectsNonAtomicStore(t *testing.T) {
	f := newAppFixture(t)
	f.edge.config.Store = struct{ NameStore }{NameStore: f.edgeStore}
	if _, err := f.exchange(t, f.signed(t, Request{Action: "allocate", Kind: "static"})); err == nil || !strings.Contains(err.Error(), "atomic") {
		t.Fatalf("non-atomic allocation error = %v", err)
	}
	if len(f.edge.state.Apps) != 0 || len(f.edgeStore.names) != 1 {
		t.Fatal("non-atomic allocation changed apps or name reservations")
	}
}
