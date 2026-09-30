package apps

import (
	"net/http"
	"strings"
)

func ambientOwnerAllowed(r *http.Request, appOrigin string) bool {
	origin := r.Header.Get("Origin")
	if origin != "" && origin != appOrigin {
		return false
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return origin == appOrigin
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none", "":
		return true
	default:
		return topLevelAppNavigation(r)
	}
}

func topLevelAppNavigation(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document" &&
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
