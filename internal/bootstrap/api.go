// Package bootstrap installs Mesh on an SSH-reachable host and verifies the
// direct Tailnet connection.
package bootstrap

import (
	"context"
	"net/http"
	"time"
)

const (
	DefaultPort          uint16 = 7337
	DefaultWebSocketPath        = "/mesh"
)

// OS names an operating system supported by the Mesh daemon.
type OS string

const (
	Linux  OS = "linux"
	Darwin OS = "darwin"
)

// Arch names a processor architecture supported by Mesh release binaries.
type Arch string

const (
	AMD64 Arch = "amd64"
	ARM64 Arch = "arm64"
)

// Platform identifies the binary needed by the remote host.
type Platform struct {
	OS   OS
	Arch Arch
}

// HostKey identifies an SSH host key that is not yet in known_hosts.
type HostKey struct {
	Host        string
	Algorithm   string
	Fingerprint string
}

// SSHOptions controls authentication and host-key trust. Empty identity files
// use the SSH agent and the standard ~/.ssh/id_* files.
type SSHOptions struct {
	KnownHostsPath string
	IdentityFiles  []string
	Password       func(context.Context, string) (string, error)
	Passphrase     func(context.Context, string) ([]byte, error)
	ConfirmHostKey func(context.Context, HostKey) (bool, error)
	ConnectTimeout time.Duration
}

// Step identifies one user-visible bootstrap operation.
type Step string

const (
	StepConnect  Step = "connect"
	StepDetect   Step = "detect"
	StepTransfer Step = "transfer"
	StepInstall  Step = "install"
	StepDiscover Step = "discover"
	StepVerify   Step = "verify"
)

// Event reports the operation that is about to run.
type Event struct {
	Step   Step
	Detail string
}

// ReleaseOptions controls cross-platform binary selection. Empty values use a
// sibling release artifact first, then the running release's exact version.
// An unversioned development build must have a matching local binary.
type ReleaseOptions struct {
	ArtifactDir string
	BaseURL     string
	Version     string
	HTTPClient  *http.Client
}

// Options describes one remote bootstrap. BinaryPath overrides automatic
// selection from the running binary, sibling artifacts, and checksum-verified
// releases.
type Options struct {
	Target           string
	BinaryPath       string
	Release          ReleaseOptions
	StateDir         string
	ExpectedIdentity string
	SSH              SSHOptions
	DaemonPort       uint16
	WebSocketPath    string
	VerifyTimeout    time.Duration
	Progress         func(Event)
}

// Result is the host observation proved by the remote daemon itself.
type Result struct {
	ID                 string
	MeshIdentity       string
	TailscaleName      string
	TailscaleAddresses []string
	Endpoint           string
	Platform           Platform
	AlreadyConfigured  bool
}

// Run converges an SSH-reachable machine on a running Mesh service, proves its
// identity over the direct WebSocket, and returns the observation for the CLI
// to persist.
func Run(ctx context.Context, opts Options) (Result, error) {
	return run(ctx, opts, defaultDependencies())
}
