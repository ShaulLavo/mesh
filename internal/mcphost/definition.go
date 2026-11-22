package mcphost

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"

	LifecycleOnDemand = "on-demand"
	LifecycleKept     = "kept"

	PolicyIsolated = "isolated"
	PolicyShared   = "shared"

	AccountNone        = "none"
	AccountHostAccount = "host-account"
	// AccountPerUser names per-user OAuth, which this release refuses so a
	// host token is never substituted for a user's own account.
	AccountPerUser = "per-user"
)

// Section 6.1 and 6.2 limits.
const (
	MaxServices          = 64
	MaxIsolatedInstances = 16
	MaxHostInstances     = 64
	maxEnvVars           = 128
	maxEnvBytes          = 32 << 10
	DefaultIdle          = 15 * time.Minute
	DefaultReadyTimeout  = 30 * time.Second
	maxReadyTimeout      = 5 * time.Minute
)

var (
	serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerName  = regexp.MustCompile(`^[!#$%&'*+.^_|~0-9A-Za-z-]+$`)
	labelText   = regexp.MustCompile(`^[\pL\pN ._@+-]{1,64}$`)
)

// ErrInvalid reports a definition that cannot be stored.
var ErrInvalid = errors.New("mcp service definition is invalid")

// Definition is one hosted MCP server as stored on its owning host. Commands,
// paths and URLs are resolved on that host.
type Definition struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`

	Transport string            `json:"transport"`
	Argv      []string          `json:"argv,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// SecretEnv maps a variable name to an absolute secret file on the host.
	SecretEnv map[string]string `json:"secretEnv,omitempty"`

	URL string `json:"url,omitempty"`
	// HeaderSecrets maps an HTTP header name to an absolute secret file.
	HeaderSecrets map[string]string `json:"headerSecrets,omitempty"`
	// RunArgv and RunCwd describe a Mesh-managed process behind URL.
	RunArgv []string `json:"runArgv,omitempty"`
	RunCwd  string   `json:"runCwd,omitempty"`

	Lifecycle      string        `json:"lifecycle"`
	Idle           time.Duration `json:"idle"`
	ReadyTimeout   time.Duration `json:"readyTimeout"`
	InstancePolicy string        `json:"instancePolicy"`
	AccountMode    string        `json:"accountMode"`
	AccountLabel   string        `json:"accountLabel,omitempty"`
	Disabled       bool          `json:"disabled,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Owned reports whether Mesh runs a process for this service.
func (d Definition) Owned() bool {
	return d.Transport == TransportStdio || len(d.RunArgv) > 0
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Normalize fills defaults and validates a definition submitted for publish.
// It never reads secret files; that happens at launch or dispatch.
func Normalize(d Definition) (Definition, error) {
	if !serviceName.MatchString(d.Name) {
		return d, invalid("name %q must be 1 to 63 lowercase letters, digits or dashes", d.Name)
	}
	d = withDefaults(d)
	checks := []func(Definition) error{validTransport, validLifecycle, validPolicy, validAccount, validEnvironment}
	for _, check := range checks {
		if err := check(d); err != nil {
			return d, fmt.Errorf("service %s: %w", d.Name, err)
		}
	}
	return d, nil
}

func withDefaults(d Definition) Definition {
	if d.Transport == "" {
		d.Transport = TransportStdio
		if d.URL != "" {
			d.Transport = TransportHTTP
		}
	}
	if d.Lifecycle == "" {
		d.Lifecycle = LifecycleOnDemand
	}
	if d.Idle == 0 && d.Lifecycle == LifecycleOnDemand {
		d.Idle = DefaultIdle
	}
	if d.ReadyTimeout == 0 {
		d.ReadyTimeout = DefaultReadyTimeout
	}
	if d.InstancePolicy == "" {
		d.InstancePolicy = PolicyIsolated
	}
	if d.AccountMode == "" {
		d.AccountMode = AccountNone
	}
	return d
}

func validTransport(d Definition) error {
	switch d.Transport {
	case TransportStdio:
		if len(d.Argv) == 0 || d.URL != "" || len(d.RunArgv) > 0 || len(d.HeaderSecrets) > 0 {
			return invalid("a stdio service needs a command and takes no URL, run command or headers")
		}
		return validDirectory(d.Cwd, "cwd")
	case TransportHTTP:
		return validHTTP(d)
	default:
		return invalid("transport must be stdio or http")
	}
}

func validHTTP(d Definition) error {
	if len(d.Argv) > 0 || len(d.SecretEnv) > 0 && len(d.RunArgv) == 0 {
		return invalid("an HTTP service takes --url, and environment secrets only with --run")
	}
	if err := LoopbackURL(d.URL); err != nil {
		return err
	}
	for name := range d.HeaderSecrets {
		if !headerName.MatchString(name) || reservedHeader(name) {
			return invalid("header %q cannot carry a credential", name)
		}
	}
	if len(d.RunArgv) == 0 {
		return nil
	}
	if d.InstancePolicy != PolicyShared {
		return invalid("a managed HTTP service owns one process at its URL and needs --shared")
	}
	return validDirectory(d.RunCwd, "run cwd")
}

func reservedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-type", "content-length", "accept", "connection", "transfer-encoding", "te", "trailer", "upgrade",
		"keep-alive", "proxy-authorization", "proxy-connection", "origin", "mcp-protocol-version", "mcp-method", "mcp-name",
		"mcp-session-id", "last-event-id":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(name), "mcp-")
	}
}

// LoopbackURL accepts only http URLs on 127.0.0.1, ::1 or localhost.
func LoopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return invalid("URL %q must be a plain http URL", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || (ip.To4() != nil && !ip.Equal(net.IPv4(127, 0, 0, 1))) {
		return invalid("URL host %q must be 127.0.0.1, ::1 or localhost", host)
	}
	return nil
}

func validDirectory(dir, what string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return invalid("%s %q must be an absolute, clean path", what, dir)
	}
	return nil
}

func validLifecycle(d Definition) error {
	if d.Lifecycle != LifecycleOnDemand && d.Lifecycle != LifecycleKept {
		return invalid("lifecycle must be on-demand or kept")
	}
	if d.Idle < 0 || d.ReadyTimeout < time.Second || d.ReadyTimeout > maxReadyTimeout {
		return invalid("idle must not be negative and the ready timeout must be 1s to 5m")
	}
	if !d.Owned() && d.Lifecycle == LifecycleKept {
		return invalid("an external HTTP service has no process to keep")
	}
	return nil
}

func validPolicy(d Definition) error {
	if d.InstancePolicy != PolicyIsolated && d.InstancePolicy != PolicyShared {
		return invalid("instance policy must be isolated or shared")
	}
	if !d.Owned() && d.InstancePolicy != PolicyShared {
		return invalid("an external HTTP service is one endpoint and needs --shared")
	}
	return nil
}

func validAccount(d Definition) error {
	switch d.AccountMode {
	case AccountPerUser:
		return invalid("per-user OAuth is not supported; Mesh will not substitute a host account for each user's own")
	case AccountNone:
		if len(d.SecretEnv) > 0 || len(d.HeaderSecrets) > 0 {
			return invalid("credentials need --account-mode host-account and an account label")
		}
		if d.AccountLabel != "" {
			return invalid("account mode none takes no account label")
		}
		return nil
	case AccountHostAccount:
		if !labelText.MatchString(d.AccountLabel) {
			return invalid("host-account mode needs a short, non-secret account label")
		}
		return nil
	default:
		return invalid("account mode must be none or host-account")
	}
}

func validEnvironment(d Definition) error {
	size := 0
	for name, value := range d.Env {
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) {
			return invalid("environment variable %q is invalid", name)
		}
		size += len(name) + len(value) + 1
	}
	for name, path := range d.SecretEnv {
		if _, plain := d.Env[name]; !envName.MatchString(name) || plain {
			return invalid("secret variable %q is invalid or also set in plain form", name)
		}
		size += len(name) + len(path) + 1
	}
	if len(d.Env)+len(d.SecretEnv) > maxEnvVars || size > maxEnvBytes {
		return invalid("at most %d variables and %d bytes of environment", maxEnvVars, maxEnvBytes)
	}
	return validSecretPaths(d)
}

func validSecretPaths(d Definition) error {
	for _, paths := range []map[string]string{d.SecretEnv, d.HeaderSecrets} {
		for _, path := range paths {
			if !filepath.IsAbs(path) {
				return invalid("secret files must be absolute paths on the host")
			}
		}
	}
	return nil
}

func newServiceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp host: create service ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
