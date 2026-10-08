package edge

import (
	"net/http"
	"strings"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func (r *Registry) filterPublicCookies(response *http.Response) error {
	cookies := response.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return nil
	}
	kept := cookies[:0]
	stripped := 0
	for _, cookie := range cookies {
		if sharedParentCookie(cookie) {
			stripped++
			continue
		}
		kept = append(kept, cookie)
	}
	if stripped != 0 {
		response.Header.Del("Set-Cookie")
		for _, cookie := range kept {
			response.Header.Add("Set-Cookie", cookie)
		}
		r.logger.Printf("edge event=cookies-stripped count=%d", stripped)
	}
	return nil
}

func sharedParentCookie(value string) bool {
	// Inspect every raw attribute: parsers disagree about duplicate Domain attributes.
	for _, attribute := range strings.Split(value, ";")[1:] {
		name, domain, ok := strings.Cut(attribute, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Domain") {
			continue
		}
		domain = strings.Trim(strings.TrimSpace(domain), "\"")
		domain = strings.Trim(domain, ".")
		for _, parent := range domainpolicy.Domains() {
			if strings.EqualFold(domain, parent) {
				return true
			}
		}
	}
	return false
}
