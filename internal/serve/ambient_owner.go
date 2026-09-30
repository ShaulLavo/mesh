package serve

import (
	"net"
	"net/http"
	"strings"
)

// WebSocketOriginPolicy distinguishes browser-facing apps from services used by
// native clients, which do not send Origin.
type WebSocketOriginPolicy uint8

const (
	RequireWebSocketOrigin WebSocketOriginPolicy = iota
	AllowOriginlessWebSocket
)

// AmbientOwnerAllowed prevents another page from borrowing private access supplied
// by the browser's network or owner credentials.
func AmbientOwnerAllowed(r *http.Request, ownOrigin string, websocketPolicy WebSocketOriginPolicy) bool {
	origin := r.Header.Get("Origin")
	if origin != "" && origin != ownOrigin {
		return false
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return origin == ownOrigin || (origin == "" && websocketPolicy == AllowOriginlessWebSocket)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none", "":
		return true
	default:
		return TopLevelNavigation(r)
	}
}

// TopLevelNavigation distinguishes intentional visits from frames and fetches.
func TopLevelNavigation(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document" &&
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func privateRequestOrigin(r *http.Request) string {
	originPrefix, defaultPort := "http://", "80"
	if r.TLS != nil {
		originPrefix, defaultPort = "https://", "443"
	}
	host := strings.ToLower(r.Host)
	if _, port, err := net.SplitHostPort(host); err == nil && port == defaultPort {
		host = strings.TrimSuffix(host, ":"+port)
	}
	return originPrefix + host
}
