package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

type configurationPolicyMismatch struct{ host string }

func (e *configurationPolicyMismatch) Error() string {
	return "reachable; service hostname is outside the loaded domain policy; reopen after saving the intended policy"
}

func structurallyValidServiceHost(host string) bool {
	canonical, ok := meshserve.CanonicalHost(host)
	if !ok || canonical != host {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	label, rest, found := strings.Cut(host, ".")
	return found && strings.Contains(rest, ".") && !meshserve.ReservedLabel(label) && label != "apps" && !(len(label) == 4 && strings.Trim(label, "0123456789abcdefghjkmnpqrstvwxyz") == "")
}
func validateServiceStructure(info protocol.ServiceInfo) error {
	if len(info.Problem) > meshserve.MaximumServiceProblemBytes || info.Healthy && info.Problem != "" || !utf8.ValidString(info.Problem) {
		return errors.New("service health is invalid")
	}
	received := protocol.ServiceFromInfo(info)
	if received.PrivateHost != "" && (received.LocalOnly || !structurallyValidServiceHost(received.PrivateHost)) {
		return errors.New("service private hostname is invalid")
	}
	received.PrivateHost = ""
	normalized, err := meshserve.Normalize(received)
	if err != nil || !normalized.Equal(received) {
		return errors.New("service definition is invalid or not canonical")
	}
	if err := validateServiceDemand(info.Demand); err != nil {
		return err
	}
	return nil
}
func validatePolicyServices(services []protocol.ServiceInfo) error {
	if !slices.ContainsFunc(services, func(service protocol.ServiceInfo) bool { return service.PrivateHost != "" }) {
		return nil
	}
	// Validate the whole observation before classifying any policy rejection.
	for _, service := range services {
		if err := validateServiceStructure(service); err != nil {
			return err
		}
	}
	for _, service := range services {
		if _, err := validateRemoteService(service); err != nil {
			return err
		}
	}
	return nil
}
func applyPolicyVerifiedState(ctx context.Context, host HostRecord, view *StateView, response protocol.Control, received time.Time, transit time.Duration) error {
	next := view.Clone()
	if err := applyVerifiedState(ctx, host, &next, response, received, transit); err != nil {
		return err
	}
	if err := validatePolicyServices(next.Services); err != nil {
		return fmt.Errorf("validate authenticated services: %w", err)
	}
	*view = next
	return nil
}
func loadedServiceHost(host string) error {
	if host == "" {
		return nil
	}
	if _, _, accepted := domainpolicy.Current().Label(host, false); !accepted {
		return &configurationPolicyMismatch{host: host}
	}
	return nil
}
