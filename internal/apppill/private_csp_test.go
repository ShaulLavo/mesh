package apppill

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPrivateFramingPolicyIntersectsWithAppPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		status     int
		policies   []string
	}{
		{name: "HTML without CSP", kind: "text/html", status: http.StatusOK},
		{name: "HTML permits all ancestors", kind: "text/html", status: http.StatusOK, policies: []string{"default-src 'self'; frame-ancestors *"}},
		{name: "HTML denies every ancestor", kind: "text/html", status: http.StatusOK, policies: []string{"default-src 'none'; frame-ancestors 'none'"}},
		{name: "HTML permits only sibling", kind: "text/html", status: http.StatusOK, policies: []string{"frame-ancestors https://zzzz.shaulavo.dev"}},
		{name: "HTML multiple policies", kind: "text/html", status: http.StatusOK, policies: []string{"frame-ancestors 'none'", "default-src 'self'; frame-ancestors *"}},
		{name: "HTML comma separated policies", kind: "text/html", status: http.StatusOK, policies: []string{"frame-ancestors 'none', frame-ancestors *"}},
		{name: "JSON without CSP", kind: "application/json", status: http.StatusOK},
		{name: "JSON with strict CSP", kind: "application/json", status: http.StatusOK, policies: []string{"frame-ancestors 'none'"}},
		{name: "no content", kind: "text/html", status: http.StatusNoContent},
		{name: "not modified", kind: "text/html", status: http.StatusNotModified},
		{name: "websocket", kind: "", status: http.StatusSwitchingProtocols},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := response("<html><head></head><body>app</body></html>", tc.kind)
			resp.StatusCode = tc.status
			for _, policy := range tc.policies {
				resp.Header.Add("Content-Security-Policy", policy)
			}
			if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev", Private: true}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			policies := resp.Header.Values("Content-Security-Policy")
			if len(policies) != len(tc.policies)+1 {
				t.Fatalf("private CSP values=%q; want original policies and one restriction", policies)
			}
			if !strings.Contains(policies[len(policies)-1], "frame-ancestors 'self'") {
				t.Fatal("missing independent same-origin framing policy")
			}
			for index, original := range tc.policies {
				for _, policy := range strings.Split(original, ",") {
					for _, directive := range strings.Split(policy, ";") {
						directive = strings.TrimSpace(directive)
						if strings.HasPrefix(directive, "frame-ancestors ") && !strings.Contains(policies[index], directive) {
							t.Fatalf("weakened app framing directive %q in %q", directive, policies[index])
						}
					}
				}
			}
			if tc.kind == "text/html" && tc.status == http.StatusOK {
				for _, value := range policies {
					for _, policy := range strings.Split(value, ",") {
						permitsManager := false
						for _, directive := range strings.Split(policy, ";") {
							fields := strings.Fields(directive)
							if len(fields) > 0 && fields[0] == "frame-src" {
								permitsManager = containsSource(fields[1:], "https://apps.shaulavo.dev")
							}
						}
						if !permitsManager {
							t.Fatalf("pill management frame is not permitted in %q", policy)
						}
					}
				}
			}
		})
	}
}

func TestPublicFramingPolicyUnchanged(t *testing.T) {
	for _, kind := range []string{"text/html", "application/json"} {
		resp := response("<html><body>public app</body></html>", kind)
		if err := Inject(resp, Config{AppID: "7k3d", ManagementOrigin: "https://apps.shaulavo.dev"}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if len(resp.Header.Values("Content-Security-Policy")) != 0 {
			t.Fatal("public app acquired a CSP")
		}
	}
}
