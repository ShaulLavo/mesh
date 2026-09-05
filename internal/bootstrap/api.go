// Package bootstrap installs Mesh on an SSH-reachable host and verifies the
// direct Tailnet connection.
package bootstrap

import (
	"context"
	"net/http"
	"time"

	"github.com/shaul/mesh/internal/wake"
)

const (
	DefaultPort          uint16 = 7337
	DefaultSSHPort       uint16 = 2222
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
	// AskUser is called when nothing named a user, the assumed one was refused,
	// and there is someone to ask. Returning an empty name declines.
	AskUser        func(ctx context.Context, target, assumed string) (string, error)
	ConnectTimeout time.Duration
}

// Step identifies one user-visible bootstrap operation.
type Step string

const (
	StepConnect   Step = "connect"
	StepDetect    Step = "detect"
	StepProvision Step = "provision"
	StepTransfer  Step = "transfer"
	StepInstall   Step = "install"
	StepDiscover  Step = "discover"
	StepVerify    Step = "verify"
)

// Event reports the operation that is about to run.
type Event struct {
	Step   Step
	Detail string
}

// ProvisionConfirmation describes the remote mutations that need explicit
// operator consent. Commands never contain an authentication key.
type ProvisionConfirmation struct {
	Summary        string
	PackageManager string
	// Actions is what an operator reads. Commands is exactly what will run,
	// kept so a test can prove the prompt does not misrepresent it.
	Actions  []ProvisionAction
	Commands []string
	Checks   []string
}

// ProvisionAction is one remote change, described plainly, with the command a
// person would type to perform it.
type ProvisionAction struct {
	Description string
	Command     string
}

// ConfirmProvisionFunc approves package installation or a system-level user
// service change on the remote host.
type ConfirmProvisionFunc func(context.Context, ProvisionConfirmation) (bool, error)

// LocalSetupFunc installs and starts Tailscale on the machine running mesh add,
// after adoption finds it cannot reach the tailnet from here. It is given the
// reason so it can tell the operator what it is fixing.
type LocalSetupFunc func(context.Context, error) error

// AuthKeyFunc supplies a Tailscale auth key for the named remote target, asked
// for only when the host turns out to need one. Bootstrap takes ownership of
// the returned bytes and clears them before Run returns.
type AuthKeyFunc func(context.Context, string) ([]byte, error)

// SudoPasswordFunc returns a sudo password for the named remote target.
// Bootstrap takes ownership of the returned bytes and clears them before Run
// returns.
type SudoPasswordFunc func(context.Context, string) ([]byte, error)

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
	TailscaleAuthKey []byte
	// TailscaleAuthKeyPrompt is asked for a key only when the remote host needs
	// one and TailscaleAuthKey is empty, so an interactive run never has to
	// fail, send the operator away for a key, and start over.
	TailscaleAuthKeyPrompt AuthKeyFunc
	// LocalTailscaleSetup offers to fix this machine rather than refusing.
	// Mesh already installs Tailscale on the host being adopted; declining to
	// do the same here is an arbitrary place to stop.
	LocalTailscaleSetup LocalSetupFunc
	ConfirmProvision    ConfirmProvisionFunc
	SudoPassword        SudoPasswordFunc
	SSH                 SSHOptions
	DaemonPort          uint16
	SSHPort             uint16
	WebSocketPath       string
	VerifyTimeout       time.Duration
	Progress            func(Event)
}

// Result is the host observation proved by the remote daemon itself.
type Result struct {
	Wake               *wake.Grant
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
