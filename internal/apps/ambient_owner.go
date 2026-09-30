package apps

import (
	"net/http"
	"strings"
)

func ambientOwnerAllowed(r *http.Request, appOrigin string) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return r.Header.Get("Origin") == appOrigin
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			return true
		default:
			origin := r.Header.Get("Origin")
			return origin == "" || origin == appOrigin
		}
	default:
		return topLevelAppNavigation(r)
	}
}

func topLevelAppNavigation(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}
