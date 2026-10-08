package daemon

import (
	"crypto/tls"
	"strings"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func privateServiceTLS(origin, services *tls.Config) *tls.Config {
	config := origin.Clone()
	config.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello != nil {
			host := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
			if _, _, accepted := domainpolicy.Label(host, false); accepted {
				return services.GetCertificate(hello)
			}
		}
		return origin.GetCertificate(hello)
	}
	return config
}
