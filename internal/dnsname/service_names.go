package dnsname

import (
	"errors"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/domainpolicy"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func privateServiceNameInDomain(label, domain string) (string, error) {
	if domain == "" {
		domain = Zone()
	}
	if !slices.Contains(domainpolicy.Domains(), domain) {
		return "", errors.New("dnsname: service domain is not configured")
	}
	host := label + "." + domain
	if err := meshserve.ValidatePrivateServiceHost(host); err != nil {
		return "", fmt.Errorf("dnsname: private service name: %w", err)
	}
	return host, nil
}
