package serve

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func (r *Registry) HasPrivateHost(host string) bool {
	snapshot := r.snapshot.Load()
	return snapshot != nil && snapshot.privateHosts[host] != nil
}

// SetPrivateHostReady configures the certificate check before serving requests.
// Each redirect checks again so renewal and expiry take effect without rebuilding routes.
func (r *Registry) SetPrivateHostReady(ready func(string) bool) {
	r.privateHostReady = ready
}

func (r *Registry) privateHostCertificateReady(host string) bool {
	return r.privateHostReady != nil && r.privateHostReady(host)
}

func redirectLegacyMount(root http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		escaped := request.URL.EscapedPath()
		if escaped != prefix && !strings.HasPrefix(escaped, prefix+"/") {
			root.ServeHTTP(w, request)
			return
		}
		target := "/" + strings.TrimLeft(strings.TrimPrefix(escaped, prefix), "/")
		if request.URL.RawQuery != "" {
			target += "?" + request.URL.RawQuery
		}
		http.Redirect(w, request, target, http.StatusTemporaryRedirect) //nolint:gosec // The target has exactly one leading slash and retains only an escaped path and query.
	})
}

func ValidatePrivateServiceHost(host string) error {
	if host == "" {
		return fmt.Errorf("serve: private service host is empty")
	}
	if err := validatePublicName(host); err != nil {
		return err
	}
	label, _, _ := domainpolicy.Label(host, false)
	if label == "apps" || len(label) == 4 && strings.Trim(label, "0123456789abcdefghjkmnpqrstvwxyz") == "" {
		return fmt.Errorf("serve: private service host %q is reserved for temporary apps", host)
	}
	return nil
}

func redirectPrivateHostNavigation(inner http.Handler, prefix, host string, ready func(string) bool) http.Handler {
	if host == "" {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		safeMethod := request.Method == http.MethodGet || request.Method == http.MethodHead
		mountRoot := request.URL.Path == prefix || request.URL.Path == prefix+"/"
		navigation := strings.Contains(request.Header.Get("Accept"), "text/html")
		if !safeMethod || !mountRoot && !navigation || request.Header.Get("Upgrade") != "" || !ready(host) {
			inner.ServeHTTP(w, request)
			return
		}
		relative := "/" + strings.TrimLeft(strings.TrimPrefix(request.URL.EscapedPath(), prefix), "/")
		target := "https://" + host + relative
		if request.URL.RawQuery != "" {
			target += "?" + request.URL.RawQuery
		}
		http.Redirect(w, request, target, http.StatusTemporaryRedirect) //nolint:gosec // The registry validates and freezes the destination hostname before publishing this handler.
	})
}

func servePrivateHost(w http.ResponseWriter, request *http.Request, host string, handler http.Handler) {
	if request.TLS != nil && request.TLS.ServerName != "" && strings.ToLower(strings.TrimSuffix(request.TLS.ServerName, ".")) != host {
		http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
		return
	}
	if !AmbientOwnerAllowed(request, privateRequestOrigin(request), AllowOriginlessWebSocket) {
		http.Error(w, "cross-site request to private service", http.StatusForbidden)
		return
	}
	handler.ServeHTTP(w, request)
}

func validatePrivateHostService(service Service) error {
	if service.PrivateHost == "" {
		return nil
	}
	if service.PublicName != "" || service.LocalOnly {
		return fmt.Errorf("serve: private host requires a tailnet-only service")
	}
	if err := ValidatePrivateServiceHost(service.PrivateHost); err != nil {
		return fmt.Errorf("serve: private host: %w", err)
	}
	return nil
}
