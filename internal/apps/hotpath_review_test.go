package apps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/shaul/mesh/internal/webauth"
)

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
				management := httptest.NewRequest(http.MethodGet, ManagementOrigin, nil)
				management.AddCookie(owner)
				switch mutation {
				case "replace":
					ticket, err := f.edge.auth.IssueView(ctx, management, identity, app.ID)
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
			f.edge.ServeHost(response, request, app.ID+"."+Domain)
			t.Logf("%s during resolution: status=%d forwards=%d", mutation, response.Code, forwarded.Load())
			if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
				t.Fatalf("invalidated view reached origin: status=%d forwards=%d", response.Code, forwarded.Load())
			}
		})
	}
}
