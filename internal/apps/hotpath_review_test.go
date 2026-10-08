package apps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type failingActivityStore struct {
	*failingEdgeStore
	attempts atomic.Int64
}

func (s *failingActivityStore) SaveAppState(ctx context.Context, key string, value []byte) error {
	if key == "apps.edge" {
		s.attempts.Add(1)
	}
	return s.failingEdgeStore.SaveAppState(ctx, key, value)
}

func TestFailedActivityFlushKeepsRestartLagBounded(t *testing.T) {
	for _, surface := range []string{"admission", "stream"} {
		t.Run(surface, func(t *testing.T) {
			f := newAppFixture(t)
			app := publicStaticApp(t, f)
			activity := func() error {
				if surface == "stream" {
					_, _, err := f.edge.activity(context.Background(), app)
					return err
				}
				_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
				if release != nil {
					release()
				}
				return err
			}
			f.now = f.now.Add(30 * time.Second)
			if err := activity(); err != nil {
				t.Fatal(err)
			}
			rt := (*f.edge.runtime.Load())[app.ID]
			accepted := rt.record.Load().ExpiresAt
			store := &failingActivityStore{failingEdgeStore: &failingEdgeStore{memoryAppStore: f.edgeStore, fail: true}}
			f.edge.config.Store = store
			for n := range 3 {
				f.now = f.now.Add(time.Minute)
				if err := activity(); !errors.Is(err, errEdgeSave) {
					t.Fatalf("failed activity %d returned %v, want storage error", n+1, err)
				}
				current := rt.record.Load().ExpiresAt
				persisted := time.Unix(0, rt.persisted.Load())
				if lag := current.Sub(persisted); lag > time.Minute {
					t.Errorf("failed activity %d left runtime %s ahead of durability", n+1, lag)
				}
				if current.Before(accepted) {
					t.Error("failed save discarded previously accepted sub-minute activity")
				}
			}
			if store.attempts.Load() != 3 {
				t.Fatalf("failed saves attempted %d times, want 3", store.attempts.Load())
			}
			restarted := f.openEdge(t)
			durable, _, err := restarted.lookup(context.Background(), app.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			lag := rt.record.Load().ExpiresAt.Sub(durable.ExpiresAt)
			t.Logf("3 failed %s saves: restart deadline lag=%s", surface, lag)
			if lag < 0 || lag > time.Minute {
				t.Errorf("restart lost %s after failed saves, want at most one minute", lag)
			}
			store.fail = false
			f.now = f.now.Add(time.Minute)
			if err := activity(); err != nil {
				t.Fatal(err)
			}
			if store.attempts.Load() != 4 || !rt.record.Load().ExpiresAt.Equal(f.now.Add(IdleTTL)) {
				t.Fatal("next activity did not retry and persist the full current deadline")
			}
		})
	}
}

func TestPendingActivityFlushKeepsRuntimeWithinDurabilitySlack(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	f.now = f.now.Add(3 * time.Minute)
	store := &pausedActivityStore{memoryAppStore: f.edgeStore, entered: make(chan struct{}), release: make(chan struct{})}
	f.edge.config.Store = store
	done := make(chan error, 1)
	go func() {
		_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
		if release != nil {
			release()
		}
		done <- err
	}()
	<-store.entered
	rt := (*f.edge.runtime.Load())[app.ID]
	lag := rt.record.Load().ExpiresAt.Sub(time.Unix(0, rt.persisted.Load()))
	if lag > time.Minute {
		t.Errorf("pending save published activity %s ahead of durability", lag)
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !rt.record.Load().ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatal("successful save did not publish the full proposed deadline")
	}
}

func TestViewCredentialMutationDuringOriginResolution(t *testing.T) {
	for _, mutation := range []string{"replace", "expire", "revoke"} {
		t.Run(mutation, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			forwarded := networkOrigin(t, f)
			owner := pairedOwner(t, f)
			view := viewCookie(t, f, owner, app.ID)
			request := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
			request.AddCookie(view)
			resolve := f.edge.config.Resolve
			f.edge.config.Resolve = func(ctx context.Context, identity string) (netip.AddrPort, error) {
				management := httptest.NewRequest(http.MethodGet, ManagementOrigin(), nil)
				management.AddCookie(owner)
				switch mutation {
				case "replace":
					nonceResponse := httptest.NewRecorder()
					nonceHash, err := f.edge.auth.BeginView(nonceResponse)
					if err != nil {
						t.Fatal(err)
					}
					request.AddCookie(cookieNamed(t, nonceResponse, webauth.ViewNonceCookie))
					ticket, err := f.edge.auth.IssueView(ctx, management, identity, app.ID, nonceHash)
					if err != nil {
						t.Fatal(err)
					}
					replacement := httptest.NewRecorder()
					if err := f.edge.auth.ConsumeView(ctx, replacement, request, ticket, app.ID); err != nil {
						t.Fatal(err)
					}
					fresh := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
					fresh.AddCookie(cookieNamed(t, replacement, webauth.ViewCookie))
					if _, err := f.edge.auth.ViewSession(ctx, fresh, app.ID); err != nil {
						t.Fatalf("replacement credential is invalid: %v", err)
					}
				case "expire":
					grant, err := f.edge.auth.ViewSession(ctx, request, app.ID)
					if err != nil {
						t.Fatal(err)
					}
					f.now = grant.ExpiresAt
				case "revoke":
					browser, err := f.edge.auth.Browser(ctx, management)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.origin.Handle(ctx, Request{Action: "browser.revoke", BrowserID: browser.ID}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.edge.auth.ViewSession(ctx, request, app.ID); !errors.Is(err, webauth.ErrUnauthorized) {
					t.Fatalf("old credential remains authorized after %s: %v", mutation, err)
				}
				return resolve(ctx, identity)
			}
			response := httptest.NewRecorder()
			f.edge.ServeHost(response, request, app.ID+"."+Domain())
			t.Logf("%s during resolution: status=%d forwards=%d", mutation, response.Code, forwarded.Load())
			if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
				t.Fatalf("invalidated view reached origin: status=%d forwards=%d", response.Code, forwarded.Load())
			}
		})
	}
}
