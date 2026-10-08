// Package domainpolicy holds immutable deployment naming policy.
package domainpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
)

type Policy struct {
	Primary                 string   `json:"primary"`
	Aliases                 []string `json:"aliases,omitempty"`
	LegacyCertificateDomain string   `json:"legacyCertificateDomain,omitempty"`
}

var active Policy
var initialize sync.Once

func Parse(reader io.Reader) (Policy, error) {
	var policy Policy
	contents, err := io.ReadAll(io.LimitReader(reader, 65537))
	if err != nil {
		return Policy{}, fmt.Errorf("domain policy: read: %w", err)
	}
	if len(contents) > 65536 {
		return Policy{}, errors.New("domain policy: configuration exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("domain policy: decode: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("domain policy: expected one JSON value")
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func (p Policy) Validate() error {
	if p.Primary == "" {
		return errors.New("domain policy: primary domain is required")
	}
	domains := p.Domains()
	if len(domains) > 8 {
		return errors.New("domain policy: at most eight domains are supported")
	}
	for index, domain := range domains {
		if err := validateDomain(domain); err != nil {
			return err
		}
		if slices.Contains(domains[:index], domain) {
			return errors.New("domain policy: duplicate domain")
		}
		for _, other := range domains[:index] {
			if strings.HasSuffix(domain, "."+other) || strings.HasSuffix(other, "."+domain) {
				return errors.New("domain policy: overlapping parent domains")
			}
		}
	}
	if p.LegacyCertificateDomain != "" && !slices.Contains(domains, p.LegacyCertificateDomain) {
		return errors.New("domain policy: legacy certificate domain must be accepted")
	}
	return nil
}

func validateDomain(domain string) error {
	if len(domain) > 240 || !strings.Contains(domain, ".") || domain != strings.ToLower(domain) {
		return errors.New("domain policy: domain must be a canonical DNS name")
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return errors.New("domain policy: domain must be a DNS name")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("domain policy: invalid DNS label")
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return errors.New("domain policy: invalid DNS character")
		}
	}
	return nil
}

// Initialize runs before command construction and before any listener starts.
// Missing policy leaves native sessions usable without enabling deployment names.
func Initialize(path string) error {
	file, err := os.Open(path) //nolint:gosec // this boundary reads the operator-selected naming configuration
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("domain policy: open: %w", err)
	}
	defer file.Close() //nolint:errcheck // parsing decides the result
	policy, err := Parse(file)
	if err != nil {
		return err
	}
	return installPolicy(policy)
}

func installPolicy(policy Policy) error {
	installed := false
	initialize.Do(func() { active = policy; installed = true })
	if !installed {
		return errors.New("domain policy: already initialized")
	}
	return nil
}

func Current() Policy {
	copyPolicy := active
	copyPolicy.Aliases = slices.Clone(active.Aliases)
	return copyPolicy
}

func (p Policy) Domains() []string {
	if p.Primary == "" {
		return nil
	}
	return append([]string{p.Primary}, p.Aliases...)
}
func Primary() string   { return active.Primary }
func Domains() []string { return active.Domains() }
func (p Policy) Label(host string, private bool) (string, string, bool) {
	for _, domain := range p.Domains() {
		suffix := "." + domain
		if private {
			suffix = ".mesh" + suffix
		}
		label, ok := strings.CutSuffix(host, suffix)
		if ok && label != "" && !strings.Contains(label, ".") {
			return label, domain, true
		}
	}
	return "", "", false
}
func Label(host string, private bool) (string, string, bool) { return active.Label(host, private) }
func Wildcard(domain string, private bool) string {
	if domain == "" {
		return ""
	}
	if private {
		return "*.mesh." + domain
	}
	return "*." + domain
}
