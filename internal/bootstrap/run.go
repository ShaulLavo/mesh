package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/identity"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultVerifyTimeout  = 10 * time.Second
	maximumClockSkew      = 5 * time.Minute
)

type remoteHost interface {
	Run(context.Context, string, io.Reader) ([]byte, []byte, error)
	Close() error
}

type dependencies struct {
	connect       func(context.Context, target, SSHOptions) (remoteHost, error)
	resolveBinary func(context.Context, binarySelection, Platform) (resolvedBinary, error)
	install       func(context.Context, remoteHost, installRequest) (bool, error)
	discover      func(context.Context, remoteHost) (tailnetObservation, error)
	checkClock    func(context.Context, remoteHost, time.Time) error
	verify        func(context.Context, []string, uint16, string) (verifiedHost, string, error)
	authorizedKey func(string) (string, error)
	now           func() time.Time
}

func defaultDependencies() dependencies {
	return dependencies{
		connect:       connectSSH,
		resolveBinary: resolvePlatformBinary,
		install:       installRemote,
		discover:      discoverTailnet,
		checkClock:    checkRemoteClock,
		verify:        verifyWebSocket,
		authorizedKey: adopterAuthorizedKey,
		now:           time.Now,
	}
}

type normalizedOptions struct {
	target           target
	binary           binarySelection
	stateDir         string
	expectedIdentity string
	ssh              SSHOptions
	daemonPort       uint16
	sshPort          uint16
	webSocketPath    string
	verifyTimeout    time.Duration
	progress         func(Event)
}

func run(ctx context.Context, opts Options, deps dependencies) (result Result, resultErr error) {
	normalized, err := normalizeOptions(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	if err := validateDependencies(deps); err != nil {
		return Result{}, err
	}

	normalized.progress(Event{Step: StepConnect, Detail: normalized.target.display()})
	remote, err := deps.connect(ctx, normalized.target, normalized.ssh)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if closeErr := remote.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("bootstrap: close SSH connection: %w", closeErr))
		}
	}()

	normalized.progress(Event{Step: StepDetect, Detail: "remote OS and architecture"})
	stdout, stderr, err := remote.Run(ctx, "uname -s; uname -m", nil)
	if err != nil {
		return Result{}, diagnostic(DiagnosticWrongArch, remoteCommandError("detect remote platform", err, stdout, stderr))
	}
	platform, err := parsePlatform(stdout)
	if err != nil {
		return Result{}, err
	}
	normalized.progress(Event{Step: StepTransfer, Detail: platform.OS.String() + "/" + platform.Arch.String() + " binary"})
	binary, err := deps.resolveBinary(ctx, normalized.binary, platform)
	if err != nil {
		return Result{}, err
	}
	defer binary.cleanup()
	if err := deps.checkClock(ctx, remote, deps.now().UTC()); err != nil {
		return Result{}, err
	}
	authorizedKey, err := deps.authorizedKey(normalized.stateDir)
	if err != nil {
		return Result{}, diagnostic(DiagnosticIdentity, fmt.Errorf("load adopter identity: %w", err))
	}

	normalized.progress(Event{Step: StepInstall, Detail: "mesh service"})
	unchanged, err := deps.install(ctx, remote, installRequest{
		Platform:      platform,
		BinaryPath:    binary.path,
		AuthorizedKey: authorizedKey,
		DaemonPort:    normalized.daemonPort,
		SSHPort:       normalized.sshPort,
		WebSocketPath: normalized.webSocketPath,
	})
	if err != nil {
		return Result{}, err
	}

	normalized.progress(Event{Step: StepDiscover, Detail: "Tailscale address"})
	tailnet, err := deps.discover(ctx, remote)
	if err != nil {
		return Result{}, err
	}
	if len(tailnet.Addresses) == 0 {
		return Result{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote host has no Tailscale addresses"))
	}

	verifyCtx, cancelVerify := context.WithTimeout(ctx, normalized.verifyTimeout)
	defer cancelVerify()
	normalized.progress(Event{Step: StepVerify, Detail: "direct WebSocket connection"})
	host, endpoint, err := deps.verify(verifyCtx, tailnet.Addresses, normalized.daemonPort, normalized.webSocketPath)
	if err != nil {
		return Result{}, err
	}
	if err := validateVerifiedHost(host, tailnet.Name); err != nil {
		return Result{}, err
	}
	if normalized.expectedIdentity != "" && host.MeshIdentity != normalized.expectedIdentity {
		return Result{}, diagnostic(DiagnosticIdentity, fmt.Errorf("daemon identity %q does not match the pinned identity %q", host.MeshIdentity, normalized.expectedIdentity))
	}
	return Result{
		ID:                 host.ID,
		MeshIdentity:       host.MeshIdentity,
		TailscaleName:      tailnet.Name,
		TailscaleAddresses: append([]string(nil), tailnet.Addresses...),
		Endpoint:           endpoint,
		Platform:           platform,
		AlreadyConfigured:  unchanged,
	}, nil
}

func normalizeOptions(ctx context.Context, opts Options) (normalizedOptions, error) {
	if ctx == nil {
		return normalizedOptions{}, errors.New("bootstrap: nil context")
	}
	if err := ctx.Err(); err != nil {
		return normalizedOptions{}, fmt.Errorf("bootstrap: %w", err)
	}
	remoteTarget, err := parseTarget(opts.Target)
	if err != nil {
		return normalizedOptions{}, err
	}
	if strings.TrimSpace(opts.StateDir) == "" {
		return normalizedOptions{}, diagnostic(DiagnosticIdentity, errors.New("local state directory is empty"))
	}
	if opts.ExpectedIdentity != "" {
		publicKey, err := base64.RawURLEncoding.DecodeString(opts.ExpectedIdentity)
		if err != nil || len(publicKey) != 32 {
			return normalizedOptions{}, diagnostic(DiagnosticIdentity, fmt.Errorf("pinned identity %q is not an Ed25519 host ID", opts.ExpectedIdentity))
		}
	}
	port := opts.DaemonPort
	if port == 0 {
		port = DefaultPort
	}
	sshPort := opts.SSHPort
	if sshPort == 0 {
		sshPort = DefaultSSHPort
	}
	webSocketPath := opts.WebSocketPath
	if webSocketPath == "" {
		webSocketPath = DefaultWebSocketPath
	}
	if err := validateWebSocketPath(webSocketPath); err != nil {
		return normalizedOptions{}, err
	}
	verifyTimeout := opts.VerifyTimeout
	if verifyTimeout == 0 {
		verifyTimeout = defaultVerifyTimeout
	}
	if verifyTimeout < 0 {
		return normalizedOptions{}, fmt.Errorf("bootstrap: verify timeout must be positive")
	}
	if opts.SSH.ConnectTimeout == 0 {
		opts.SSH.ConnectTimeout = defaultConnectTimeout
	}
	if opts.SSH.ConnectTimeout < 0 {
		return normalizedOptions{}, fmt.Errorf("bootstrap: SSH connect timeout must be positive")
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(Event) {}
	}
	return normalizedOptions{
		target: remoteTarget,
		binary: binarySelection{
			explicitPath: opts.BinaryPath,
			artifactDir:  opts.Release.ArtifactDir,
			baseURL:      opts.Release.BaseURL,
			version:      opts.Release.Version,
			httpClient:   opts.Release.HTTPClient,
		},
		stateDir:         opts.StateDir,
		expectedIdentity: opts.ExpectedIdentity,
		ssh:              opts.SSH,
		daemonPort:       port,
		sshPort:          sshPort,
		webSocketPath:    webSocketPath,
		verifyTimeout:    verifyTimeout,
		progress:         progress,
	}, nil
}

func validateDependencies(deps dependencies) error {
	if deps.connect == nil || deps.resolveBinary == nil || deps.install == nil || deps.discover == nil || deps.checkClock == nil || deps.verify == nil || deps.authorizedKey == nil || deps.now == nil {
		return errors.New("bootstrap: incomplete dependencies")
	}
	return nil
}

func validateWebSocketPath(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || value == "" || value[0] != '/' || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || path.Clean(value) != value {
		return fmt.Errorf("bootstrap: WebSocket path %q must be a clean absolute path without a query or fragment", value)
	}
	for _, r := range value {
		if !(r == '/' || r == '-' || r == '_' || r == '.' || r == '~' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("bootstrap: WebSocket path %q contains a character unsupported by service files", value)
		}
	}
	return nil
}

func adopterAuthorizedKey(stateDir string) (string, error) {
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return "", err
	}
	publicKey, err := ssh.NewPublicKey(host.PublicKey)
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))), nil
}

type verifiedHost struct {
	ID            string
	MeshIdentity  string
	TailscaleName string
}

func validateVerifiedHost(host verifiedHost, discoveredName string) error {
	if host.ID == "" || host.MeshIdentity == "" || host.ID != host.MeshIdentity {
		return diagnostic(DiagnosticIdentity, fmt.Errorf("daemon reported inconsistent host identity %q / %q", host.ID, host.MeshIdentity))
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(host.ID)
	if err != nil || len(publicKey) != 32 {
		return diagnostic(DiagnosticIdentity, fmt.Errorf("daemon reported invalid Ed25519 host ID %q", host.ID))
	}
	if host.TailscaleName != "" && discoveredName != "" && strings.TrimSuffix(host.TailscaleName, ".") != strings.TrimSuffix(discoveredName, ".") {
		return diagnostic(DiagnosticIdentity, fmt.Errorf("daemon reports Tailscale name %q, but tailscale status reports %q", host.TailscaleName, discoveredName))
	}
	return nil
}

func remoteCommandError(operation string, runErr error, stdout, stderr []byte) error {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(stdout))
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, runErr)
	}
	return fmt.Errorf("%s: %w: %s", operation, runErr, detail)
}

func (o OS) String() string   { return string(o) }
func (a Arch) String() string { return string(a) }
