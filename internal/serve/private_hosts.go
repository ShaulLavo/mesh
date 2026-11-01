package serve

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func (r *Registry) HasPrivateHost(host string) bool {
	snapshot := r.snapshot.Load()
	private, ok := snapshotPrivateRoute(snapshot, host)
	return ok && private.handler != nil
}

func snapshotPrivateRoute(snapshot *registrySnapshot, host string) (privateServiceRoute, bool) {
	if snapshot == nil {
		return privateServiceRoute{}, false
	}
	private, ok := snapshot.privateHosts[host]
	return private, ok
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
	if err := ValidateDeploymentHost(host); err != nil {
		return err
	}
	label, _, _ := domainpolicy.Label(host, false)
	if label == "apps" || len(label) == 4 && strings.Trim(label, "0123456789abcdefghjkmnpqrstvwxyz") == "" {
		return fmt.Errorf("serve: private service host %q is reserved for temporary apps", host)
	}
	return nil
}

func servePrivateHost(w http.ResponseWriter, request *http.Request, host string, private privateServiceRoute) {
	if request.TLS != nil && request.TLS.ServerName != "" && strings.ToLower(strings.TrimSuffix(request.TLS.ServerName, ".")) != host {
		http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
		return
	}
	if len(request.Header.Values("Origin")) > 1 {
		http.Error(w, "multiple Origin headers are forbidden", http.StatusForbidden)
		return
	}
	if !trustedOriginAllowed(request, private.trustedOrigins) && !AmbientOwnerAllowed(request, privateRequestOrigin(request), AllowOriginlessWebSocket) {
		http.Error(w, "cross-site request to private service", http.StatusForbidden)
		return
	}
	private.handler.ServeHTTP(w, request)
}

func trustedOriginAllowed(request *http.Request, trustedOrigins []string) bool {
	origin := request.Header.Get("Origin")
	return origin != "" && slices.Contains(trustedOrigins, origin)
}

func validatePrivateHostService(service Service) error {
	if service.PrivateHost == "" {
		if len(service.TrustedOrigins) > 0 {
			return fmt.Errorf("serve: trusted origins require a private host")
		}
		return nil
	}
	if service.LocalOnly {
		return fmt.Errorf("serve: private host requires a tailnet-only service")
	}
	if err := ValidatePrivateServiceHost(service.PrivateHost); err != nil {
		return fmt.Errorf("serve: private host: %w", err)
	}
	return nil
}
