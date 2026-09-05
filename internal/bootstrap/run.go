package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/user"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/wake"
)

const (
	defaultConnectTimeout        = 10 * time.Second
	defaultVerifyTimeout         = 10 * time.Second
	maximumClockSkew             = 5 * time.Minute
	maximumAuthKeyBytes          = 64 << 10
	maximumRemoteDiagnosticBytes = 4 << 10
)

type remoteHost interface {
	Run(context.Context, string, io.Reader, ...remoteOutputLimits) ([]byte, []byte, error)
	Close() error
}

type dependencies struct {
	connect       func(context.Context, target, SSHOptions) (remoteHost, error)
	localTailnet  func(context.Context) error
	resolveBinary func(context.Context, binarySelection, Platform) (resolvedBinary, error)
	install       func(context.Context, remoteHost, installRequest) (bool, error)
	discover      func(context.Context, remoteHost) (tailscaleObservation, error)
	provision     func(context.Context, remoteHost, provisionRequest) (provisionResult, error)
	checkClock    func(context.Context, remoteHost, time.Time) error
	verify        func(context.Context, []string, uint16, string) (verifiedHost, string, error)
	authorizedKey func(string) (string, error)
	now           func() time.Time
}

func defaultDependencies() dependencies {
	return dependencies{
		connect:       connectSSH,
		localTailnet:  checkLocalTailnet,
		resolveBinary: resolvePlatformBinary,
		install:       installRemote,
		discover:      discoverTailnet,
		provision:     provisionRemote,
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
	tailscaleAuthKey []byte
	confirmProvision ConfirmProvisionFunc
	sudoPassword     SudoPasswordFunc
	ssh              SSHOptions
	daemonPort       uint16
	sshPort          uint16
	webSocketPath    string
	verifyTimeout    time.Duration
	progress         func(Event)
}

func run(ctx context.Context, opts Options, deps dependencies) (result Result, resultErr error) {
	authKeyForRedaction := bytes.TrimSpace(opts.TailscaleAuthKey)
	// A key typed at the prompt has to be redacted exactly like one read from a
	// file, and it does not exist yet when the deferred redaction is installed.
	var promptedAuthKey AuthKeyFunc
	if opts.TailscaleAuthKeyPrompt != nil {
		promptedAuthKey = func(ctx context.Context, target string) ([]byte, error) {
			key, err := opts.TailscaleAuthKeyPrompt(ctx, target)
			if trimmed := bytes.TrimSpace(key); len(trimmed) > 0 {
				authKeyForRedaction = trimmed
			}
			return key, err
		}
	}
	defer func() {
		if resultContainsAuthKey(result, authKeyForRedaction) {
			result = Result{}
			resultErr = diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote response contained the supplied Tailscale auth key; result was discarded"))
		}
		resultErr = redactAuthKey(resultErr, authKeyForRedaction)
	}()
	normalized, err := normalizeOptions(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	defer clear(normalized.tailscaleAuthKey)
	if err := validateDependencies(deps); err != nil {
		return Result{}, err
	}
	// Adoption ends by dialling the host over the tailnet from here, so a local
	// machine that is not on it cannot finish no matter what happens remotely.
	// Checking last meant installing and starting Tailscale on someone else's
	// computer and only then discovering this one could never reach it.
	if err := deps.localTailnet(ctx); err != nil {
		if opts.LocalTailscaleSetup == nil {
			return Result{}, diagnostic(DiagnosticLocalTailscale, err)
		}
		if setupErr := opts.LocalTailscaleSetup(ctx, err); setupErr != nil {
			return Result{}, diagnostic(DiagnosticLocalTailscale, setupErr)
		}
		if err := deps.localTailnet(ctx); err != nil {
			return Result{}, diagnostic(DiagnosticLocalTailscale, err)
		}
	}

	// Checked before the connection is attempted, not after it fails: the
	// host-key prompt cancels the context on its way out.
	running, alreadyServing := probeRunningDaemon(ctx, normalized, deps)

	normalized.progress(Event{Step: StepConnect, Detail: normalized.target.display()})
	remote, err := deps.connect(ctx, normalized.target, normalized.ssh)
	if err != nil && shouldAskForUser(normalized.target, normalized.ssh, err) {
		// The name came from this machine's own account because nothing named
		// one. Asking beats failing with advice: the operator knows the account
		// and is standing right here.
		name, askErr := normalized.ssh.AskUser(ctx, normalized.target.alias, normalized.target.user)
		if askErr == nil && strings.TrimSpace(name) != "" {
			normalized.target.user = strings.TrimSpace(name)
			normalized.target.guessedUser = false
			normalized.target.explicitUser = true
			normalized.progress(Event{Step: StepConnect, Detail: normalized.target.display()})
			remote, err = deps.connect(ctx, normalized.target, normalized.ssh)
		}
	}
	if err != nil {
		// A machine that refuses SSH may still be serving Mesh already, and then
		// there is nothing to install and no reason to need SSH at all. Falling
		// back only here keeps SSH the way a bare machine is adopted. The SSH
		// failure is the one reported if this finds nothing, because a host that
		// is not running Mesh needed that connection.
		if alreadyServing {
			normalized.progress(Event{Step: StepVerify, Detail: "already serving Mesh, adopted over the tailnet"})
			return running, nil
		}
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

	normalized.progress(Event{Step: StepDiscover, Detail: "Tailscale status and address"})
	tailscale, err := deps.discover(ctx, remote)
	if err != nil {
		return Result{}, err
	}
	if detail := tailscale.Variant.detail(); detail != "" && tailscale.State != tailscaleMissing {
		normalized.progress(Event{Step: StepDiscover, Detail: "found " + detail})
	}
	provisioned, err := deps.provision(ctx, remote, provisionRequest{
		Platform:      platform,
		Target:        normalized.target.display(),
		Observation:   tailscale,
		AuthKey:       normalized.tailscaleAuthKey,
		AuthKeyPrompt: promptedAuthKey,
		Confirm:       normalized.confirmProvision,
		SudoPassword:  normalized.sudoPassword,
		Progress:      normalized.progress,
	})
	if err != nil {
		return Result{}, err
	}
	tailnet := provisioned.Tailnet
	if stringsContainAuthKey(append([]string{tailnet.Name}, tailnet.Addresses...), normalized.tailscaleAuthKey) {
		return Result{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote Tailscale status contained the supplied auth key; Mesh was not installed"))
	}
	if len(tailnet.Addresses) == 0 {
		return Result{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote host has no Tailscale addresses"))
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
		Progress:      normalized.progress,
	})
	if err != nil {
		return Result{}, err
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
		Wake:               host.Wake,
		ID:                 host.ID,
		MeshIdentity:       host.MeshIdentity,
		TailscaleName:      tailnet.Name,
		TailscaleAddresses: append([]string(nil), tailnet.Addresses...),
		Endpoint:           endpoint,
		Platform:           platform,
		AlreadyConfigured:  unchanged && !provisioned.Changed,
	}, nil
}

func redactAuthKey(err error, authKey []byte) error {
	return redactSecret(err, authKey)
}

func redactSecret(err error, secret []byte) error {
	if err == nil || len(secret) == 0 || !strings.Contains(err.Error(), string(secret)) {
		return err
	}
	if diagnosticError, ok := err.(*DiagnosticError); ok {
		redactedCause := redactSecret(diagnosticError.Err, secret)
		if redactedCause == nil {
			redactedCause = errors.New("[redacted]")
		}
		return &DiagnosticError{
			Code:       diagnosticError.Code,
			Suggestion: diagnosticError.Suggestion,
			Err:        redactedCause,
		}
	}
	type multiUnwrapper interface {
		Unwrap() []error
	}
	if joined, ok := err.(multiUnwrapper); ok {
		causes := joined.Unwrap()
		redacted := make([]error, len(causes))
		for index, cause := range causes {
			redacted[index] = redactSecret(cause, secret)
		}
		return errors.Join(redacted...)
	}
	return errors.New(strings.ReplaceAll(err.Error(), string(secret), "[redacted]"))
}

func resultContainsAuthKey(result Result, authKey []byte) bool {
	values := []string{result.ID, result.MeshIdentity, result.TailscaleName, result.Endpoint}
	values = append(values, result.TailscaleAddresses...)
	return stringsContainAuthKey(values, authKey)
}

func stringsContainAuthKey(values []string, authKey []byte) bool {
	if len(authKey) == 0 {
		return false
	}
	secret := string(authKey)
	for _, value := range values {
		if strings.Contains(value, secret) {
			return true
		}
	}
	return false
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
	// Mesh dials the address itself, so a Host alias only ssh knows about would
	// otherwise fail as a DNS lookup for the alias.
	sshConfig := userSSHConfig()
	remoteTarget = applySSHConfig(remoteTarget, sshConfig)
	// An explicitly named identity wins; otherwise take the one the config
	// names for this alias, the way ssh would.
	if len(opts.SSH.IdentityFiles) == 0 {
		if identity := sshConfigIdentityFile(remoteTarget.alias, sshConfig); identity != "" {
			opts.SSH.IdentityFiles = []string{identity}
		}
	}
	// Last, so it cannot beat a User the ssh config named. `mesh add pi` on a
	// config with `User pi` must connect as pi, not as whoever is typing.
	if remoteTarget.user == "" {
		current, err := user.Current()
		if err != nil || strings.TrimSpace(current.Username) == "" {
			return normalizedOptions{}, diagnostic(DiagnosticInvalidTarget, fmt.Errorf("target %q names no user, ~/.ssh/config sets none for %q, and this machine has no username; use user@host", opts.Target, remoteTarget.alias))
		}
		remoteTarget.user = strings.TrimSpace(current.Username)
		remoteTarget.guessedUser = true
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
	authKey := bytes.TrimSpace(opts.TailscaleAuthKey)
	if len(authKey) > maximumAuthKeyBytes {
		return normalizedOptions{}, errors.New("bootstrap: Tailscale auth key file is too large")
	}
	if len(authKey) != 0 {
		authKey = append([]byte(nil), authKey...)
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
		tailscaleAuthKey: authKey,
		confirmProvision: opts.ConfirmProvision,
		sudoPassword:     opts.SudoPassword,
		ssh:              opts.SSH,
		daemonPort:       port,
		sshPort:          sshPort,
		webSocketPath:    webSocketPath,
		verifyTimeout:    verifyTimeout,
		progress:         progress,
	}, nil
}

// checkLocalTailnet reports whether this machine can reach the tailnet at all.
func checkLocalTailnet(ctx context.Context) error {
	self, err := tailnet.Self(ctx)
	if err != nil {
		return err
	}
	if len(self.Addrs) == 0 {
		return errors.New("this machine has no Tailscale address")
	}
	return nil
}

func validateDependencies(deps dependencies) error {
	if deps.connect == nil || deps.localTailnet == nil || deps.resolveBinary == nil || deps.install == nil || deps.discover == nil || deps.provision == nil || deps.checkClock == nil || deps.verify == nil || deps.authorizedKey == nil || deps.now == nil {
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
	Wake          *wake.Grant
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
	detail := boundedRemoteOutput(stderr)
	if detail == "" {
		detail = boundedRemoteOutput(stdout)
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, runErr)
	}
	return fmt.Errorf("%s: %w: %s", operation, runErr, detail)
}

func boundedRemoteOutput(output []byte) string {
	output = bytes.TrimSpace(output)
	truncated := len(output) > maximumRemoteDiagnosticBytes
	if truncated {
		output = output[:maximumRemoteDiagnosticBytes]
	}
	detail := strings.ToValidUTF8(string(output), "�")
	if truncated {
		detail += " … [truncated]"
	}
	return detail
}

func (o OS) String() string   { return string(o) }
func (a Arch) String() string { return string(a) }

// shouldAskForUser reports whether a failed connection is worth one more try
// under a name the operator supplies. Only an authentication failure against a
// user nobody chose qualifies: a refused host key or an unreachable host is not
// fixed by a different account.
func shouldAskForUser(t target, opts SSHOptions, err error) bool {
	if !t.guessedUser || opts.AskUser == nil {
		return false
	}
	var diagnostic *DiagnosticError
	return errors.As(err, &diagnostic) && diagnostic.Code == DiagnosticSSHAuth
}
