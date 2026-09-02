package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	// The immutable source URL and digest keep the fallback from becoming a
	// remote curl-to-shell path when a host has no supported package manager.
	pinnedTailscaleInstallerURL    = "https://raw.githubusercontent.com/tailscale/tailscale/11a6255b22a3071bb63992ee8f7fbedd6d50f4d1/scripts/installer.sh"
	pinnedTailscaleInstallerSHA256 = "805e85ed6f6f81a7ea2e70d52d47e7d5290863299e5c922b2787d71aa312f22e"
	maximumInstallerBytes          = 1 << 20
	maximumRepositoryAssetBytes    = 1 << 20
)

const linuxUserServiceProbeCommand = `if ! command -v systemctl >/dev/null 2>&1 || ! command -v loginctl >/dev/null 2>&1; then printf 'MESH_ERROR=no_systemd\n'; exit 0; fi; remote_user=$(id -un) || exit 1; remote_uid=$(id -u) || exit 1; printf 'MESH_USER=%s\nMESH_UID=%s\n' "$remote_user" "$remote_uid"; loginctl show-user "$remote_user" --property=Linger`

const brewPathProbeCommand = `
brew_path=$(command -v brew 2>/dev/null || true)
if [ -n "$brew_path" ] && [ -x "$brew_path" ]; then
	printf 'MESH_BREW_PATH=%s\n' "$brew_path"
elif [ -x /opt/homebrew/bin/brew ]; then
	printf 'MESH_BREW_PATH=/opt/homebrew/bin/brew\n'
elif [ -x /usr/local/bin/brew ]; then
	printf 'MESH_BREW_PATH=/usr/local/bin/brew\n'
else
	printf 'MESH_BREW_MISSING=yes\n'
fi`

const linuxInstallMethodCommand = `
if [ -r /etc/os-release ]; then
	. /etc/os-release
	distro_ids=" ${ID:-} ${ID_LIKE:-} "
	distro_name=${PRETTY_NAME:-${NAME:-${ID:-}}}
	case "$distro_ids" in
		*" arch "*|*" manjaro "*|*" endeavouros "*|*" garuda "*)
			if command -v pacman >/dev/null 2>&1; then
				printf 'MESH_INSTALL_METHOD=pacman\nMESH_DISTRO=%s\n' "$distro_name"
				exit 0
			fi
			;;
	esac

	downloader=
	if command -v curl >/dev/null 2>&1; then
		downloader=curl
	elif command -v wget >/dev/null 2>&1; then
		downloader=wget
	fi
	repo_os=
	repo_version=
	case "${ID:-}" in
		ubuntu|pop|neon|tuxedo|elementary|zorin)
			repo_os=ubuntu
			repo_version=${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}
			;;
		debian|raspbian)
			repo_os=$ID
			repo_version=${VERSION_CODENAME:-}
			;;
		linuxmint)
			if [ -n "${UBUNTU_CODENAME:-}" ]; then
				repo_os=ubuntu
				repo_version=$UBUNTU_CODENAME
			elif [ -n "${DEBIAN_CODENAME:-}" ]; then
				repo_os=debian
				repo_version=$DEBIAN_CODENAME
			fi
			;;
		*)
			case "$distro_ids" in
				*" ubuntu "*) repo_os=ubuntu; repo_version=${UBUNTU_CODENAME:-${VERSION_CODENAME:-}} ;;
				*" debian "*) repo_os=debian; repo_version=${DEBIAN_CODENAME:-${VERSION_CODENAME:-}} ;;
			esac
			;;
	esac
	if command -v apt-get >/dev/null 2>&1 && [ -n "$repo_os" ] && [ -n "$repo_version" ] && [ -n "$downloader" ]; then
		printf 'MESH_INSTALL_METHOD=apt\nMESH_REPO_OS=%s\nMESH_REPO_VERSION=%s\nMESH_DOWNLOADER=%s\nMESH_DISTRO=%s\n' "$repo_os" "$repo_version" "$downloader" "$distro_name"
		exit 0
	fi
fi

if command -v curl >/dev/null 2>&1; then
	printf 'MESH_INSTALL_METHOD=script\nMESH_DOWNLOADER=curl\n'
elif command -v wget >/dev/null 2>&1; then
	printf 'MESH_INSTALL_METHOD=script\nMESH_DOWNLOADER=wget\n'
else
	printf 'MESH_INSTALL_METHOD=none\n'
fi`

type provisionRequest struct {
	Platform      Platform
	Target        string
	Observation   tailscaleObservation
	AuthKey       []byte
	AuthKeyPrompt AuthKeyFunc
	Confirm       ConfirmProvisionFunc
	SudoPassword  SudoPasswordFunc
	Progress      func(Event)
}

type provisionResult struct {
	Tailnet tailnetObservation
	Changed bool
}

type installKind uint8

const (
	installNone installKind = iota
	installPacman
	installAPT
	installHomebrew
	installPinnedScript
)

type installPlan struct {
	Kind            installKind
	Distro          string
	RepoOS          string
	Codename        string
	Downloader      string
	BrewPath        string
	InstallerURL    string
	InstallerSHA256 string
}

type installStepKind uint8

const (
	installStepCommand installStepKind = iota
	installStepDownloadAPTKey
	installStepDownloadAPTList
	installStepWriteAPTKey
	installStepWriteAPTList
	installStepDownloadInstaller
	installStepExecuteInstaller
)

type installStep struct {
	Kind                      installStepKind
	Operation                 string
	Command                   string
	Privileged                bool
	MayMutate                 bool
	PackageInstalledOnSuccess bool
}

type installOutcome struct {
	Changed          bool
	PackageInstalled bool
}

func (p installPlan) packageManager() string {
	switch p.Kind {
	case installPacman:
		return "pacman"
	case installAPT:
		return "apt"
	case installHomebrew:
		return "Homebrew"
	case installPinnedScript:
		return "the checksum-verified Tailscale installer"
	case installNone:
		return ""
	default:
		return "unknown"
	}
}

func (p installPlan) steps() []installStep {
	switch p.Kind {
	case installPacman:
		return []installStep{
			{Operation: "install Tailscale with pacman", Command: "pacman -S --needed --noconfirm tailscale", Privileged: true, MayMutate: true, PackageInstalledOnSuccess: true},
			{Operation: "start the Tailscale daemon", Command: "systemctl enable --now tailscaled", Privileged: true, MayMutate: true},
		}
	case installAPT:
		keyURL, listURL := p.aptURLs()
		return []installStep{
			{Kind: installStepDownloadAPTKey, Operation: "download the Tailscale apt key", Command: p.downloadURLCommand(keyURL)},
			{Kind: installStepDownloadAPTList, Operation: "download the Tailscale apt repository", Command: p.downloadURLCommand(listURL)},
			{Operation: "create the apt keyring directory", Command: "mkdir -p --mode=0755 /usr/share/keyrings", Privileged: true, MayMutate: true},
			{Kind: installStepWriteAPTKey, Operation: "install the Tailscale apt key", Command: "tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null", Privileged: true, MayMutate: true},
			{Kind: installStepWriteAPTList, Operation: "install the Tailscale apt repository", Command: "tee /etc/apt/sources.list.d/tailscale.list >/dev/null", Privileged: true, MayMutate: true},
			{Operation: "set Tailscale apt repository permissions", Command: "chmod 0644 /usr/share/keyrings/tailscale-archive-keyring.gpg /etc/apt/sources.list.d/tailscale.list", Privileged: true, MayMutate: true},
			{Operation: "update apt package indexes", Command: "apt-get update", Privileged: true, MayMutate: true},
			{Operation: "install Tailscale with apt", Command: "env DEBIAN_FRONTEND=noninteractive apt-get install -y tailscale", Privileged: true, MayMutate: true, PackageInstalledOnSuccess: true},
			{Operation: "start the Tailscale daemon", Command: "systemctl enable --now tailscaled", Privileged: true, MayMutate: true},
		}
	case installHomebrew:
		brew := shellQuote(p.BrewPath)
		return []installStep{
			{Operation: "install Tailscale with Homebrew", Command: brew + " install tailscale", MayMutate: true, PackageInstalledOnSuccess: true},
			{Operation: "start the Tailscale daemon", Command: brew + " services start tailscale", Privileged: true, MayMutate: true},
		}
	case installPinnedScript:
		return []installStep{
			{Kind: installStepDownloadInstaller, Operation: "download the pinned Tailscale installer", Command: p.downloadCommand()},
			{Kind: installStepExecuteInstaller, Operation: "run the checksum-verified Tailscale installer", Command: "/bin/sh -s", Privileged: true, MayMutate: true, PackageInstalledOnSuccess: true},
		}
	case installNone:
		return nil
	default:
		return nil
	}
}

func (p installPlan) renderedCommands(privilege privilegeSpec) []string {
	steps := p.steps()
	commands := make([]string, 0, len(steps))
	for _, step := range steps {
		command := step.Command
		if step.Privileged {
			command = privilege.command(command)
		}
		commands = append(commands, command)
	}
	return commands
}

func (p installPlan) actions(privilege privilegeSpec) []ProvisionAction {
	steps := p.steps()
	actions := make([]ProvisionAction, 0, len(steps))
	for _, step := range steps {
		command := step.Command
		if step.Privileged {
			command = privilege.displayCommand(command)
		}
		actions = append(actions, ProvisionAction{Description: step.Operation, Command: command})
	}
	return actions
}

func (p installPlan) checks() []string {
	if p.Kind != installPinnedScript {
		return nil
	}
	return []string{"SHA-256 must equal " + p.InstallerSHA256 + " before /bin/sh runs"}
}

func (p installPlan) aptURLs() (string, string) {
	base := "https://pkgs.tailscale.com/stable/" + p.RepoOS + "/" + p.Codename
	return base + ".noarmor.gpg", base + ".tailscale-keyring.list"
}

func (p installPlan) downloadCommand() string {
	return p.downloadURLCommand(p.InstallerURL)
}

func (p installPlan) downloadURLCommand(url string) string {
	if p.Downloader == "wget" {
		return "wget -qO- " + shellQuote(url)
	}
	return "curl -fsSL " + shellQuote(url)
}

type linuxUserService struct {
	User      string
	UID       uint64
	Lingering bool
}

func provisionRemote(ctx context.Context, remote remoteHost, request provisionRequest) (result provisionResult, resultErr error) {
	if request.Progress == nil {
		request.Progress = func(Event) {}
	}
	var access privilege
	defer func() {
		if stringsContainAuthKey(append([]string{result.Tailnet.Name}, result.Tailnet.Addresses...), access.Password) {
			result = provisionResult{}
			resultErr = diagnostic(DiagnosticSudoAuth, errors.New("remote response contained the supplied sudo password; result was discarded"))
		}
		resultErr = redactSecret(resultErr, access.Password)
		access.clear()
	}()
	current := request.Observation
	switch current.State {
	case tailscaleNeedsMachineAuth:
		return provisionResult{}, machineAuthDiagnostic()
	case tailscaleNeedsLogin, tailscaleNoState:
		if len(request.AuthKey) == 0 {
			key, err := requestAuthKey(ctx, &request, current.State)
			if err != nil {
				return provisionResult{}, err
			}
			request.AuthKey = key
		}
	}

	var linuxService *linuxUserService
	var uid uint64
	if request.Platform.OS == Linux {
		service, err := inspectLinuxUserService(ctx, remote)
		if err != nil {
			return provisionResult{}, err
		}
		linuxService = &service
		uid = service.UID
	}

	install := installPlan{Kind: installNone}
	if current.State == tailscaleMissing {
		var err error
		install, err = detectInstallPlan(ctx, remote, request.Platform)
		if err != nil {
			return provisionResult{}, err
		}
	}

	enableLingering := linuxService != nil && !linuxService.Lingering
	brewPath := install.BrewPath
	if request.Platform.OS == Darwin && current.State == tailscaleDaemonStopped && current.Variant != tailscaleVariantApplication && brewPath == "" {
		var err error
		brewPath, err = detectBrewPath(ctx, remote)
		if err != nil {
			return provisionResult{}, err
		}
	}
	needsPrivilege := provisionNeedsPrivilege(request.Platform, current, install, enableLingering)
	accessSpec := privilegeSpec{Mode: privilegeRoot}
	if needsPrivilege {
		if request.Platform.OS == Darwin {
			var err error
			uid, err = remoteUID(ctx, remote)
			if err != nil {
				return provisionResult{}, err
			}
		}
		var err error
		accessSpec, err = detectPrivilege(ctx, remote, uid)
		if err != nil {
			return provisionResult{}, err
		}
	}
	consentRequired := install.Kind != installNone || enableLingering
	if consentRequired {
		confirmation := buildProvisionConfirmation(request, install, linuxService, accessSpec)
		if request.Confirm == nil {
			return provisionResult{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote provisioning needs confirmation; retry with --yes"))
		}
		confirmed, err := request.Confirm(ctx, confirmation)
		if err != nil {
			return provisionResult{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("confirm remote provisioning: %w", err))
		}
		if !confirmed {
			return provisionResult{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote provisioning was declined"))
		}
	}
	var err error
	access, err = accessSpec.acquire(ctx, remote, request.Target, request.SudoPassword)
	if err != nil {
		return provisionResult{}, err
	}

	changed := false
	installed := false
	if install.Kind != installNone {
		request.Progress(Event{Step: StepProvision, Detail: "install Tailscale with " + install.packageManager()})
		outcome, err := applyInstallPlan(ctx, remote, install, access)
		installed = outcome.PackageInstalled
		if err != nil {
			return provisionResult{}, err
		}
		changed = true
		current, err = discoverTailnet(ctx, remote)
		if err != nil {
			return provisionResult{}, installedProvisionFailure("verify Tailscale after installation", err)
		}
	}

	converged, stateChanged, err := convergeTailscale(ctx, remote, request, current, access, brewPath, installed)
	if err != nil {
		return provisionResult{}, err
	}
	changed = changed || stateChanged

	if enableLingering {
		request.Progress(Event{Step: StepProvision, Detail: "enable systemd user lingering"})
		if err := enableUserLingering(ctx, remote, *linuxService, access); err != nil {
			if installed {
				return provisionResult{}, diagnostic(DiagnosticNoUserLingering, fmt.Errorf("Tailscale was installed, but user lingering could not be enabled; Mesh was not installed: %w", diagnosticCause(err)))
			}
			return provisionResult{}, err
		}
		changed = true
	}
	return provisionResult{Tailnet: converged.Tailnet, Changed: changed}, nil
}

func buildProvisionConfirmation(request provisionRequest, install installPlan, service *linuxUserService, privilege privilegeSpec) ProvisionConfirmation {
	actions := install.actions(privilege)
	commands := install.renderedCommands(privilege)
	checks := install.checks()
	if privilege.Mode == privilegeSudoPassword {
		checks = append(checks, "sudo password must authenticate before any remote change")
	}
	manager := install.packageManager()
	summary := "Change remote system settings?"
	if install.Kind != installNone {
		if install.Distro != "" {
			summary = fmt.Sprintf("%s is running %s and has no Tailscale. Install it with %s?", request.Target, install.Distro, manager)
		} else {
			summary = fmt.Sprintf("%s has no Tailscale. Install it with %s?", request.Target, manager)
		}
	} else {
		manager = "loginctl"
		if service != nil {
			summary = fmt.Sprintf("Enable systemd user lingering for %s on %s?", service.User, request.Target)
		}
	}
	if service != nil && !service.Lingering {
		actions = append(actions, ProvisionAction{
			Description: "keep " + service.User + "'s services running after logout",
			Command:     privilege.displayCommand("loginctl enable-linger " + shellQuote(service.User)),
		})
		actions = append(actions, ProvisionAction{
			Description: "confirm lingering is on",
			Command:     "loginctl show-user " + shellQuote(service.User) + " --property=Linger",
		})
		commands = append(commands, privilege.command("loginctl enable-linger "+shellQuote(service.User)))
		commands = append(commands, "loginctl show-user "+shellQuote(service.User)+" --property=Linger")
	}
	return ProvisionConfirmation{
		Summary:        summary,
		PackageManager: manager,
		Actions:        append([]ProvisionAction(nil), actions...),
		Commands:       append([]string(nil), commands...),
		Checks:         append([]string(nil), checks...),
	}
}

func provisionNeedsPrivilege(platform Platform, current tailscaleObservation, install installPlan, enableLingering bool) bool {
	if install.Kind != installNone || enableLingering {
		return true
	}
	switch current.State {
	case tailscaleDaemonStopped, tailscaleStopped, tailscaleNeedsLogin, tailscaleNoState:
		return !(platform.OS == Darwin && current.Variant == tailscaleVariantApplication)
	default:
		return false
	}
}

func installedProvisionFailure(operation string, err error) error {
	code := DiagnosticTailscaleUnavailable
	var diagnosticError *DiagnosticError
	if errors.As(err, &diagnosticError) {
		code = diagnosticError.Code
	}
	return diagnostic(code, fmt.Errorf("Tailscale was installed, but %s failed; Mesh was not installed: %w", operation, diagnosticCause(err)))
}

func convergeTailscale(ctx context.Context, remote remoteHost, request provisionRequest, current tailscaleObservation, access privilege, brewPath string, installed bool) (_ tailscaleObservation, changed bool, resultErr error) {
	defer func() {
		if installed && resultErr != nil {
			resultErr = installedProvisionFailure("finish Tailscale setup", resultErr)
		}
	}()
	authAttempted := false
	for transitions := 0; transitions < 4; transitions++ {
		switch current.State {
		case tailscaleRunning:
			return current, changed, nil
		case tailscaleMissing:
			if installed {
				return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale is still missing after its installer completed"))
			}
			return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale is not installed"))
		case tailscaleDaemonStopped:
			if request.Platform.OS == Darwin {
				installedApp, err := darwinApplicationInstalled(ctx, remote)
				if err != nil {
					return tailscaleObservation{}, false, err
				}
				if installedApp {
					return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("the existing Tailscale application is not responding; open Tailscale.app and retry"))
				}
			}
			request.Progress(Event{Step: StepProvision, Detail: "start the Tailscale daemon"})
			command := tailscaleDaemonStartCommand(request.Platform, brewPath)
			if _, _, err := access.run(ctx, remote, "start the Tailscale daemon", command, nil, false, DiagnosticTailscaleUnavailable); err != nil {
				return tailscaleObservation{}, false, err
			}
			changed = true
		case tailscaleStopped:
			request.Progress(Event{Step: StepProvision, Detail: "bring the Tailscale backend up"})
			if err := runTailscaleUp(ctx, remote, request.Platform, current, access, nil); err != nil {
				return tailscaleObservation{}, false, err
			}
			changed = true
		case tailscaleNeedsLogin, tailscaleNoState:
			if authAttempted {
				return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleLoggedOut, fmt.Errorf("Tailscale authentication completed, but the backend state remained %s; the auth key was not retained", current.State))
			}
			if len(request.AuthKey) == 0 {
				key, err := requestAuthKey(ctx, &request, current.State)
				if err != nil {
					return tailscaleObservation{}, false, err
				}
				request.AuthKey = key
			}
			request.Progress(Event{Step: StepProvision, Detail: "authenticate Tailscale from standard input"})
			if err := runTailscaleUp(ctx, remote, request.Platform, current, access, request.AuthKey); err != nil {
				return tailscaleObservation{}, false, err
			}
			authAttempted = true
			changed = true
		case tailscaleNeedsMachineAuth:
			return tailscaleObservation{}, false, machineAuthDiagnostic()
		case tailscaleStarting:
			return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale backend state remained Starting"))
		default:
			return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported Tailscale state %s", current.State))
		}

		observation, err := discoverTailnet(ctx, remote)
		if err != nil {
			if authAttempted {
				message := "Tailscale authentication completed, but status verification failed; the auth key was not retained"
				return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleLoggedOut, errors.New(message))
			}
			return tailscaleObservation{}, false, err
		}
		current = observation
	}
	return tailscaleObservation{}, false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale did not reach Running after bounded state transitions; last state was %s", current.State))
}

func inspectLinuxUserService(ctx context.Context, remote remoteHost) (linuxUserService, error) {
	stdout, stderr, err := remote.Run(ctx, linuxUserServiceProbeCommand, nil)
	if err != nil {
		return linuxUserService{}, diagnostic(DiagnosticNoSystemd, remoteCommandError("inspect remote user service", err, stdout, stderr))
	}
	values := markerValues(stdout)
	if values["MESH_ERROR"] == "no_systemd" {
		return linuxUserService{}, diagnostic(DiagnosticNoSystemd, errors.New("systemctl or loginctl is not installed"))
	}
	user := values["MESH_USER"]
	uid, err := strconv.ParseUint(values["MESH_UID"], 10, 64)
	if user == "" || err != nil {
		return linuxUserService{}, diagnostic(DiagnosticNoSystemd, fmt.Errorf("parse remote user service probe %q", boundedRemoteOutput(stdout)))
	}
	linger, ok := values["Linger"]
	if !ok || linger != "yes" && linger != "no" {
		return linuxUserService{}, diagnostic(DiagnosticNoUserLingering, fmt.Errorf("loginctl returned no valid Linger property for %s", user))
	}
	return linuxUserService{User: user, UID: uid, Lingering: linger == "yes"}, nil
}

func remoteUID(ctx context.Context, remote remoteHost) (uint64, error) {
	stdout, stderr, err := remote.Run(ctx, "id -u", nil)
	if err != nil {
		return 0, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("read remote user ID", err, stdout, stderr))
	}
	uidText := strings.TrimSpace(string(stdout))
	uid, err := strconv.ParseUint(uidText, 10, 64)
	if err != nil {
		return 0, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("parse remote user ID %q", boundedRemoteOutput([]byte(uidText))))
	}
	return uid, nil
}

func markerValues(output []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func detectInstallPlan(ctx context.Context, remote remoteHost, platform Platform) (installPlan, error) {
	switch platform.OS {
	case Linux:
		return detectLinuxInstallPlan(ctx, remote)
	case Darwin:
		installedApp, err := darwinApplicationInstalled(ctx, remote)
		if err != nil {
			return installPlan{}, err
		}
		if installedApp {
			return installPlan{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale.app is installed but its bundled CLI could not report status; open the application and retry"))
		}
		brewPath, err := detectBrewPath(ctx, remote)
		if err != nil {
			return installPlan{}, err
		}
		return installPlan{Kind: installHomebrew, BrewPath: brewPath}, nil
	default:
		return installPlan{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("no Tailscale provisioner for %s", platform.OS))
	}
}

func detectBrewPath(ctx context.Context, remote remoteHost) (string, error) {
	stdout, stderr, err := remote.Run(ctx, brewPathProbeCommand, nil)
	if err != nil {
		return "", diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("find Homebrew", err, stdout, stderr))
	}
	values := markerValues(stdout)
	missing, hasMissing := values["MESH_BREW_MISSING"]
	if hasMissing && missing != "yes" {
		return "", diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Homebrew probe returned invalid result %q", boundedRemoteOutput(stdout)))
	}
	if missing == "yes" {
		if values["MESH_BREW_PATH"] != "" {
			return "", diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Homebrew probe returned conflicting result %q", boundedRemoteOutput(stdout)))
		}
		return "", diagnostic(DiagnosticTailscaleUnavailable, errors.New("Homebrew is not installed on the remote Mac"))
	}
	brewPath := values["MESH_BREW_PATH"]
	if !validRemoteExecutablePath(brewPath) {
		return "", diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Homebrew probe returned invalid result %q", boundedRemoteOutput(stdout)))
	}
	return brewPath, nil
}

func detectLinuxInstallPlan(ctx context.Context, remote remoteHost) (installPlan, error) {
	stdout, stderr, err := remote.Run(ctx, linuxInstallMethodCommand, nil)
	if err != nil {
		return installPlan{}, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("detect remote package manager", err, stdout, stderr))
	}
	values := markerValues(stdout)
	switch values["MESH_INSTALL_METHOD"] {
	case "pacman":
		return installPlan{Kind: installPacman, Distro: displayDistro(values["MESH_DISTRO"])}, nil
	case "apt":
		repoOS := values["MESH_REPO_OS"]
		codename := values["MESH_REPO_VERSION"]
		downloader := values["MESH_DOWNLOADER"]
		if validRepositoryToken(repoOS) && validRepositoryToken(codename) && (downloader == "curl" || downloader == "wget") {
			return installPlan{Kind: installAPT, RepoOS: repoOS, Codename: codename, Downloader: downloader, Distro: displayDistro(values["MESH_DISTRO"])}, nil
		}
	case "script":
		downloader := values["MESH_DOWNLOADER"]
		if downloader == "curl" || downloader == "wget" {
			return installPlan{Kind: installPinnedScript, Downloader: downloader, InstallerURL: pinnedTailscaleInstallerURL, InstallerSHA256: pinnedTailscaleInstallerSHA256}, nil
		}
	case "none":
		return installPlan{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("remote host has no supported package manager, curl, or wget"))
	}
	return installPlan{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported package-manager probe result %q", boundedRemoteOutput(stdout)))
}

// displayDistro keeps a PRETTY_NAME short enough to sit in one prompt line and
// drops anything that is not plain printable text, since it comes off a remote
// host and lands in a terminal.
func displayDistro(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 40 {
		value = value[:40]
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func validRemoteExecutablePath(value string) bool {
	if value == "" || value[0] != '/' || path.Clean(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validRepositoryToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '.' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func applyInstallPlan(ctx context.Context, remote remoteHost, plan installPlan, access privilege) (installOutcome, error) {
	if plan.Kind == installNone {
		return installOutcome{}, nil
	}
	if len(plan.steps()) == 0 {
		return installOutcome{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("unsupported Tailscale installation method"))
	}
	var outcome installOutcome
	var aptKey []byte
	var aptList []byte
	var installer []byte
	for _, step := range plan.steps() {
		var payload []byte
		switch step.Kind {
		case installStepWriteAPTKey:
			payload = aptKey
		case installStepWriteAPTList:
			payload = aptList
		case installStepExecuteInstaller:
			digest := fmt.Sprintf("%x", sha256.Sum256(installer))
			if digest != plan.InstallerSHA256 {
				return outcome, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("downloaded Tailscale installer SHA-256 is %s, want %s; installer was not executed", digest, plan.InstallerSHA256))
			}
			payload = installer
		}

		if step.MayMutate {
			outcome.Changed = true
		}
		var stdout []byte
		var err error
		limits := []remoteOutputLimits(nil)
		switch step.Kind {
		case installStepDownloadAPTKey, installStepDownloadAPTList:
			limits = []remoteOutputLimits{{Stdout: maximumRepositoryAssetBytes + 1}}
		case installStepDownloadInstaller:
			limits = []remoteOutputLimits{{Stdout: maximumInstallerBytes + 1}}
		}
		if step.Privileged {
			stdout, _, err = access.run(ctx, remote, step.Operation, step.Command, payload, false, DiagnosticTailscaleUnavailable, limits...)
		} else {
			stdout, _, err = (privilege{Spec: privilegeSpec{Mode: privilegeRoot}}).run(ctx, remote, step.Operation, step.Command, payload, false, DiagnosticTailscaleUnavailable, limits...)
		}
		if err != nil {
			return outcome, installStepFailure(outcome, step, err)
		}
		if step.PackageInstalledOnSuccess {
			outcome.PackageInstalled = true
		}
		switch step.Kind {
		case installStepDownloadAPTKey:
			if err := validateDownloadSize("Tailscale repository key", stdout, maximumRepositoryAssetBytes); err != nil {
				return outcome, err
			}
			aptKey = stdout
		case installStepDownloadAPTList:
			if err := validateDownloadSize("Tailscale repository list", stdout, maximumRepositoryAssetBytes); err != nil {
				return outcome, err
			}
			aptList = stdout
		case installStepDownloadInstaller:
			if err := validateDownloadSize("Tailscale installer", stdout, maximumInstallerBytes); err != nil {
				return outcome, err
			}
			installer = stdout
		}
	}
	return outcome, nil
}

func validateDownloadSize(name string, contents []byte, maximum int) error {
	if len(contents) == 0 || len(contents) > maximum {
		return diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("downloaded %s has invalid size %d", name, len(contents)))
	}
	return nil
}

func installStepFailure(outcome installOutcome, step installStep, err error) error {
	cause := diagnosticCause(err)
	switch {
	case outcome.PackageInstalled:
		return diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale was installed, but %s failed; Mesh was not installed: %w", step.Operation, cause))
	case outcome.Changed:
		return diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale provisioning may have changed the remote host, but %s failed; Mesh was not installed: %w", step.Operation, cause))
	default:
		return err
	}
}

func diagnosticCause(err error) error {
	var diagnosticError *DiagnosticError
	if errors.As(err, &diagnosticError) {
		return diagnosticError.Err
	}
	return err
}

func runTailscaleUp(ctx context.Context, remote remoteHost, platform Platform, observation tailscaleObservation, access privilege, authKey []byte) error {
	withAuth := len(authKey) != 0
	var payload []byte
	if withAuth {
		payload = make([]byte, len(authKey)+1)
		copy(payload, authKey)
		payload[len(payload)-1] = '\n'
		defer clear(payload)
	}
	step := tailscaleUpStep(platform, observation, withAuth)
	runner := access
	if !step.Privileged {
		runner = privilege{Spec: privilegeSpec{Mode: privilegeRoot}}
	}
	code := DiagnosticTailscaleUnavailable
	if withAuth {
		code = DiagnosticTailscaleLoggedOut
	}
	_, _, err := runner.run(ctx, remote, "run tailscale up", step.Command, payload, withAuth, code)
	if err == nil {
		return nil
	}
	if withAuth {
		return diagnostic(DiagnosticTailscaleLoggedOut, fmt.Errorf("remote tailscale up failed while reading the auth key from standard input; the key was not retained: %w", diagnosticCause(err)))
	}
	return err
}

func tailscaleUpStep(platform Platform, observation tailscaleObservation, withAuth bool) installStep {
	arguments := "up"
	if withAuth {
		arguments += " --auth-key=file:/dev/stdin"
	}
	if platform.OS == Darwin && observation.Variant == tailscaleVariantApplication {
		cliPath := observation.CLIPath
		if cliPath == "" {
			cliPath = tailscaleApplicationCLI
		}
		return installStep{Operation: "run tailscale up", Command: "/usr/bin/env TAILSCALE_BE_CLI=1 " + shellQuote(cliPath) + " " + arguments}
	}
	cli := "tailscale"
	if observation.CLIPath != "" {
		cli = shellQuote(observation.CLIPath)
	}
	return installStep{Operation: "run tailscale up", Command: cli + " " + arguments, Privileged: true}
}

func tailscaleUpCommand(platform Platform, observation tailscaleObservation, privilege privilegeSpec, withAuth bool) string {
	step := tailscaleUpStep(platform, observation, withAuth)
	if step.Privileged {
		return privilege.command(step.Command)
	}
	return step.Command
}

func tailscaleDaemonStartCommand(platform Platform, brewPath string) string {
	if platform.OS == Darwin {
		return shellQuote(brewPath) + " services start tailscale"
	}
	return "systemctl enable --now tailscaled"
}

func enableUserLingering(ctx context.Context, remote remoteHost, service linuxUserService, access privilege) error {
	command := "loginctl enable-linger " + shellQuote(service.User)
	_, _, err := access.run(ctx, remote, "enable remote user lingering", command, nil, false, DiagnosticNoUserLingering)
	if err != nil {
		return err
	}
	verifyCommand := "loginctl show-user " + shellQuote(service.User) + " --property=Linger"
	stdout, stderr, err := remote.Run(ctx, verifyCommand, nil)
	if err != nil {
		return diagnostic(DiagnosticNoUserLingering, remoteCommandError("verify remote user lingering", err, stdout, stderr))
	}
	if markerValues(stdout)["Linger"] != "yes" {
		return diagnostic(DiagnosticNoUserLingering, fmt.Errorf("loginctl did not report Linger=yes for %s", service.User))
	}
	return nil
}

func darwinApplicationInstalled(ctx context.Context, remote remoteHost) (bool, error) {
	command := "if [ -d '/Applications/Tailscale.app' ]; then printf 'yes\\n'; else printf 'no\\n'; fi"
	stdout, stderr, err := remote.Run(ctx, command, nil)
	if err != nil {
		return false, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("inspect Tailscale application", err, stdout, stderr))
	}
	switch strings.TrimSpace(string(stdout)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale application probe returned %q", boundedRemoteOutput(stdout)))
	}
}

// requestAuthKey asks for a key at the moment it is needed. Failing here and
// telling the operator to fetch a key and run the whole adoption again is the
// worst version of this: the remote host has already changed by then.
func requestAuthKey(ctx context.Context, request *provisionRequest, state tailscaleState) ([]byte, error) {
	if request.AuthKeyPrompt == nil {
		return nil, missingAuthKeyDiagnostic(state)
	}
	key, err := request.AuthKeyPrompt(ctx, request.Target)
	if err != nil {
		return nil, diagnostic(DiagnosticTailscaleLoggedOut, err)
	}
	key = bytes.TrimSpace(key)
	if len(key) == 0 {
		return nil, missingAuthKeyDiagnostic(state)
	}
	return key, nil
}

func missingAuthKeyDiagnostic(state tailscaleState) error {
	return diagnostic(DiagnosticTailscaleLoggedOut, fmt.Errorf("Tailscale backend state is %s; provide --tailscale-auth-key-file", state))
}

func machineAuthDiagnostic() error {
	return diagnostic(DiagnosticTailscaleMachineAuth, errors.New("Tailscale backend state is NeedsMachineAuth; an administrator must approve this machine in the Tailscale admin console"))
}
