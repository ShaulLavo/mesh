package serve

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaximumTrustedOrigins     = 16
	MaximumTrustedOriginBytes = 300
	originHTTPScheme          = "https"
	originHTTPSchemePort      = "443"
	originHTTP                = "http"
	originHTTPPort            = "80"
)

// CanonicalOrigin returns the exact origin spelling Mesh stores and compares.
// It deliberately accepts only origins that a browser can serialize
// unambiguously, rather than trying to interpret browser-specific host syntax.
func CanonicalOrigin(value string) (string, error) {
	if err := validateOriginInput(value); err != nil {
		return "", err
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse origin: %w", err)
	}
	if err := validateParsedOrigin(parsed); err != nil {
		return "", err
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != originHTTP && scheme != originHTTPScheme {
		return "", fmt.Errorf("origin scheme %q is unsupported", parsed.Scheme)
	}
	host, port, err := canonicalOriginAuthority(parsed.Host)
	if err != nil {
		return "", err
	}
	if scheme == originHTTP && !httpTrustedHost(host) {
		return "", errors.New("http origins are allowed only for loopback hosts")
	}
	canonical := formatCanonicalOrigin(scheme, host, port)
	if len(canonical) > MaximumTrustedOriginBytes {
		return "", fmt.Errorf("origin exceeds %d bytes", MaximumTrustedOriginBytes)
	}
	return canonical, nil
}

func validateOriginInput(value string) error {
	if value == "" || len(value) > MaximumTrustedOriginBytes || !utf8.ValidString(value) {
		return errors.New("origin is empty, too long, or not valid UTF-8")
	}
	if strings.ContainsAny(value, "\\%?#") || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return errors.New("origin contains unsupported syntax")
	}
	return nil
}

func validateParsedOrigin(parsed *url.URL) error {
	if parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil {
		return errors.New("origin must be scheme://host[:port]")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return errors.New("origin may not contain a path, query, or fragment")
	}
	return nil
}

// NormalizeTrustedOrigins validates, canonicalizes, sorts, and deduplicates a
// caller-owned list without retaining its backing array.
func NormalizeTrustedOrigins(origins []string) ([]string, error) {
	if len(origins) == 0 {
		return nil, nil
	}
	if len(origins) > MaximumTrustedOrigins {
		return nil, fmt.Errorf("trusted origins exceed %d entries", MaximumTrustedOrigins)
	}
	input := slices.Clone(origins)
	canonical := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, origin := range input {
		value, err := CanonicalOrigin(origin)
		if err != nil {
			return nil, fmt.Errorf("trusted origin %q: %w", origin, err)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		canonical = append(canonical, value)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func canonicalOriginAuthority(authority string) (string, string, error) {
	if strings.HasSuffix(authority, ":") {
		return "", "", errors.New("origin has an empty port")
	}
	switch {
	case strings.HasPrefix(authority, "["):
		return canonicalBracketedAuthority(authority)
	case strings.Count(authority, ":") == 1:
		parts := strings.SplitN(authority, ":", 2)
		return canonicalOriginHostPort(parts[0], parts[1])
	case strings.Contains(authority, ":"):
		return "", "", errors.New("origin has an invalid host")
	default:
		return canonicalOriginHostPort(authority, "")
	}
}

func canonicalBracketedAuthority(authority string) (string, string, error) {
	closeIndex := strings.IndexByte(authority, ']')
	if closeIndex < 0 {
		return "", "", errors.New("origin has an invalid IPv6 host")
	}
	host := authority[1:closeIndex]
	remainder := authority[closeIndex+1:]
	if remainder == "" {
		return canonicalOriginHostPort(host, "")
	}
	if !strings.HasPrefix(remainder, ":") || remainder[1:] == "" {
		return "", "", errors.New("origin has an invalid port")
	}
	return canonicalOriginHostPort(host, remainder[1:])
}

func canonicalOriginHostPort(host, port string) (string, string, error) {
	if host == "" || strings.HasSuffix(host, ".") || strings.Contains(host, "*") {
		return "", "", errors.New("origin has an invalid host")
	}
	for _, character := range host {
		if character > unicode.MaxASCII {
			return "", "", errors.New("origin host must be ASCII")
		}
	}
	canonicalHost, err := canonicalOriginHost(host)
	if err != nil {
		return "", "", err
	}
	canonicalPort, err := canonicalOriginPort(port)
	if err != nil {
		return "", "", err
	}
	return canonicalHost, canonicalPort, nil
}

func canonicalOriginPort(port string) (string, error) {
	if port == "" {
		return "", nil
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", errors.New("origin has an invalid port")
	}
	return strconv.FormatUint(number, 10), nil
}

func canonicalOriginHost(host string) (string, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Is4In6() {
			return "", errors.New("IPv4-mapped IPv6 origins are unsupported")
		}
		return address.String(), nil
	}
	if looksLikeNumericHost(host) {
		return "", errors.New("alternate numeric host spellings are unsupported")
	}
	lower := strings.ToLower(host)
	if len(lower) > 253 {
		return "", errors.New("origin host is too long")
	}
	for _, label := range strings.Split(lower, ".") {
		if !validHostLabel(label) {
			return "", errors.New("origin host is invalid")
		}
	}
	return lower, nil
}

func formatCanonicalOrigin(scheme, host, port string) string {
	if port == defaultOriginPort(scheme) {
		port = ""
	}
	if port != "" {
		if strings.Contains(host, ":") {
			host = net.JoinHostPort(host, port)
		} else {
			host += ":" + port
		}
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

func looksLikeNumericHost(host string) bool {
	if strings.HasPrefix(strings.ToLower(host), "0x") {
		return true
	}
	for _, character := range host {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func httpTrustedHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func defaultOriginPort(scheme string) string {
	if scheme == originHTTP {
		return originHTTPPort
	}
	return originHTTPSchemePort
}
