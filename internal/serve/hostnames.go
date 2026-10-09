package serve

import (
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func ValidateDeploymentHost(name string) error {
	if name == "" {
		return fmt.Errorf("deployment host is empty")
	}
	if len(name) > 253 || name != strings.ToLower(name) || strings.HasSuffix(name, ".") {
		return fmt.Errorf("deployment host %q is not a canonical hostname", name)
	}
	label, _, accepted := domainpolicy.Label(name, false)
	if !accepted {
		return fmt.Errorf("deployment host %q is outside configured deployment domains", name)
	}
	if ReservedLabel(label) {
		return fmt.Errorf("deployment host %q is reserved for private naming", name)
	}
	for _, label := range strings.Split(name, ".") {
		if !validDNSLabel(label) {
			return fmt.Errorf("deployment host %q has an invalid DNS label", name)
		}
	}
	return nil
}
func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		letter := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if !letter && !digit && character != '-' {
			return false
		}
	}
	return true
}
