package bootstrap

import "fmt"

// DiagnosticCode identifies a bootstrap failure without requiring text
// matching by callers.
type DiagnosticCode string

const (
	DiagnosticInvalidTarget        DiagnosticCode = "invalid_target"
	DiagnosticSSHConnect           DiagnosticCode = "ssh_connect"
	DiagnosticSSHAuth              DiagnosticCode = "ssh_auth"
	DiagnosticSSHHostKey           DiagnosticCode = "ssh_host_key"
	DiagnosticWrongArch            DiagnosticCode = "wrong_arch"
	DiagnosticNoSystemd            DiagnosticCode = "no_systemd"
	DiagnosticNoUserLingering      DiagnosticCode = "no_user_lingering"
	DiagnosticServiceInstall       DiagnosticCode = "service_install"
	DiagnosticTailscaleUnavailable DiagnosticCode = "tailscale_unavailable"
	DiagnosticTailscaleLoggedOut   DiagnosticCode = "tailscale_logged_out"
	DiagnosticPortBlocked          DiagnosticCode = "port_blocked"
	DiagnosticClockSkew            DiagnosticCode = "clock_skew"
	DiagnosticIdentity             DiagnosticCode = "identity_verification"
)

// DiagnosticError gives a failed operation a stable name and a recovery step.
type DiagnosticError struct {
	Code       DiagnosticCode
	Suggestion string
	Err        error
}

func (e *DiagnosticError) Error() string {
	return fmt.Sprintf("bootstrap %s: %v; fix: %s", e.Code, e.Err, e.Suggestion)
}

func (e *DiagnosticError) Unwrap() error { return e.Err }

func diagnostic(code DiagnosticCode, err error) error {
	return &DiagnosticError{Code: code, Suggestion: diagnosticSuggestion(code), Err: err}
}

func diagnosticSuggestion(code DiagnosticCode) string {
	switch code {
	case DiagnosticInvalidTarget:
		return "use user@host or user@host:port"
	case DiagnosticSSHConnect:
		return "run ssh user@host and check the host name, SSH port, and network route"
	case DiagnosticSSHAuth:
		return "run ssh user@host and add a working key to the SSH agent"
	case DiagnosticSSHHostKey:
		return "run ssh user@host, verify the fingerprint, and update ~/.ssh/known_hosts"
	case DiagnosticWrongArch:
		return "build or download Mesh for the reported OS and architecture, then retry with that binary"
	case DiagnosticNoSystemd:
		return "install systemd with user services, then run systemctl --user status"
	case DiagnosticNoUserLingering:
		return "run sudo loginctl enable-linger $USER on the remote host"
	case DiagnosticServiceInstall:
		return "inspect the remote mesh service with systemctl --user status mesh or launchctl print gui/$(id -u)/dev.shaulavo.mesh"
	case DiagnosticTailscaleUnavailable:
		return "install and start Tailscale on the remote host"
	case DiagnosticTailscaleLoggedOut:
		return "run tailscale up on the remote host"
	case DiagnosticPortBlocked:
		return "run tailscale ping for the host, then allow the Mesh TCP port in the tailnet ACL and host firewall"
	case DiagnosticClockSkew:
		return "enable network time with timedatectl set-ntp true or sudo sntp -sS time.apple.com"
	case DiagnosticIdentity:
		return "restart the remote mesh service and retry; do not replace identity.key"
	default:
		return "retry with the reported operation fixed"
	}
}
