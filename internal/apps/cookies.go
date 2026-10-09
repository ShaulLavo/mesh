package apps

import (
	"net/http"
	"strings"
)

// App responses must not replace the registry's authorization cookies.
func stripCookies(header http.Header) {
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if !reservedCookie(strings.TrimSpace(name)) {
			header.Add("Set-Cookie", value)
		}
	}
}

func reservedCookie(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "__host-mesh") || strings.HasPrefix(strings.ToLower(name), "__secure-mesh")
}

// App code must not receive Mesh owner credentials.
func stripRequestCookies(request *http.Request) {
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if !reservedCookie(cookie.Name) {
			request.AddCookie(cookie)
		}
	}
}
