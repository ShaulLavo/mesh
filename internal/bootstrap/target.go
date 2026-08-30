package bootstrap

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type target struct {
	user string
	host string
	port uint16
}

func (t target) address() string {
	return net.JoinHostPort(t.host, strconv.Itoa(int(t.port)))
}

func (t target) display() string {
	host := t.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.port == 22 {
		return t.user + "@" + host
	}
	return fmt.Sprintf("%s@%s:%d", t.user, host, t.port)
}

func parseTarget(raw string) (target, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q is empty or contains outer whitespace", raw))
	}
	parsed, err := url.Parse("ssh://" + raw)
	if err != nil {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("parse target %q: %w", raw, err))
	}
	if parsed.Scheme != "ssh" || parsed.User == nil || parsed.User.Username() == "" || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q must be user@host or user@host:port", raw))
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q contains a password", raw))
	}
	port := uint64(22)
	if text := parsed.Port(); text != "" {
		port, err = strconv.ParseUint(text, 10, 16)
		if err != nil || port == 0 {
			return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q has an invalid SSH port", raw))
		}
	}
	return target{user: parsed.User.Username(), host: parsed.Hostname(), port: uint16(port)}, nil
}
