package apps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestPrivateAppEdgeErrorsRetainIsolationHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{
		{name: "cross-page denied", code: http.StatusForbidden},
		{name: "app starting", code: http.StatusServiceUnavailable},
		{name: "origin resolution fails", code: http.StatusServiceUnavailable},
		{name: "origin unreachable", code: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			authenticate := ambientOwnerRequest(t, f, app, "network")
			r := httptest.NewRequest(http.MethodGet, URL(app.ID)+"/", nil)
			authenticate(r)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Origin", URL(app.ID))
			switch tc.name {
			case "cross-page denied":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("Origin", "https://attacker.example")
			case "app starting":
				f.edge.mu.Lock()
				record := f.edge.state.Apps[app.ID]
				record.Ready = false
				f.edge.state.Apps[app.ID] = record
				f.edge.publishRuntime(f.edge.state, nil)
				f.edge.mu.Unlock()
			case "origin resolution fails":
				f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) {
					return netip.AddrPort{}, errors.New("fixture origin offline")
				}
			case "origin unreachable":
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				endpoint, err := netip.ParseAddrPort(listener.Addr().String())
				if err != nil {
					_ = listener.Close()
					t.Fatal(err)
				}
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil }
			}
			w := httptest.NewRecorder()
			f.edge.ServeHost(w, r, app.ID+"."+Domain)
			if w.Code != tc.code {
				t.Fatalf("private error status=%d; want %d", w.Code, tc.code)
			}
			for key, want := range map[string]string{
				"Cross-Origin-Resource-Policy": "same-origin",
				"X-Frame-Options":              "SAMEORIGIN",
				"Content-Security-Policy":      "frame-ancestors 'self'",
			} {
				if got := w.Header().Values(key); len(got) != 1 || got[0] != want {
					t.Errorf("private edge error %s=%q; want one %q policy", key, got, want)
				}
			}
		})
	}
}
