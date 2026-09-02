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

	// alias is the host exactly as typed, before ~/.ssh/config is applied.
	// The explicit flags record what the caller supplied, because a command
	// line always beats the config file.
	alias        string
	explicitUser bool
	explicitPort bool
}

func (t target) address() string {
	return net.JoinHostPort(t.host, strconv.Itoa(int(t.port)))
}

func (t target) display() string {
	host := t.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	address := t.user + "@" + host
	if t.port != 22 {
		address = fmt.Sprintf("%s@%s:%d", t.user, host, t.port)
	}
	// After ~/.ssh/config resolves an alias the address is not what was typed,
	// and an operator approving remote changes has to recognise the machine.
	if t.alias != "" && t.alias != t.host {
		return t.alias + " (" + address + ")"
	}
	return address
}

func parseTarget(raw string) (target, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q is empty or contains outer whitespace", raw))
	}
	parsed, err := url.Parse("ssh://" + raw)
	if err != nil {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("parse target %q: %w", raw, err))
	}
	if parsed.Scheme != "ssh" || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q must be host, user@host, or user@host:port", raw))
	}
	user := ""
	if parsed.User != nil {
		if parsed.User.Username() == "" {
			return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q has an empty user before @", raw))
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q contains a password", raw))
		}
		user = parsed.User.Username()
	}
	port := uint64(22)
	explicitPort := parsed.Port() != ""
	if explicitPort {
		port, err = strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil || port == 0 {
			return target{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q has an invalid SSH port", raw))
		}
	}
	return target{
		user:         user,
		host:         parsed.Hostname(),
		port:         uint16(port),
		alias:        parsed.Hostname(),
		explicitUser: user != "",
		explicitPort: explicitPort,
	}, nil
}
