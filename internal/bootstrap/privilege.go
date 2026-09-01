package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

const (
	maximumSudoPasswordBytes = 64 << 10
)

const sudoCapabilityProbeCommand = `
sudo_path=$(command -v sudo 2>/dev/null || true)
if [ -z "$sudo_path" ]; then
	printf 'MESH_SUDO_MODE=missing\n'
elif "$sudo_path" -n true >/dev/null 2>&1; then
	printf 'MESH_SUDO_MODE=noninteractive\nMESH_SUDO_PATH=%s\n' "$sudo_path"
else
	printf 'MESH_SUDO_MODE=password\nMESH_SUDO_PATH=%s\n' "$sudo_path"
fi`

type privilegeMode uint8

const (
	privilegeRoot privilegeMode = iota
	privilegeSudoNonInteractive
	privilegeSudoPassword
)

type privilegeSpec struct {
	Mode     privilegeMode
	SudoPath string
}

type privilege struct {
	Spec     privilegeSpec
	Password []byte
}

func detectPrivilege(ctx context.Context, remote remoteHost, uid uint64) (privilegeSpec, error) {
	if uid == 0 {
		return privilegeSpec{Mode: privilegeRoot}, nil
	}
	stdout, stderr, err := remote.Run(ctx, sudoCapabilityProbeCommand, nil)
	if err != nil {
		return privilegeSpec{}, diagnostic(DiagnosticSudoAuth, remoteCommandError("inspect remote sudo access", err, stdout, stderr))
	}
	values := markerValues(stdout)
	mode := values["MESH_SUDO_MODE"]
	if mode == "missing" {
		return privilegeSpec{}, diagnostic(DiagnosticSudoAuth, errors.New("sudo is not installed on the remote host"))
	}
	sudoPath := values["MESH_SUDO_PATH"]
	if !validRemoteExecutablePath(sudoPath) {
		return privilegeSpec{}, diagnostic(DiagnosticSudoAuth, fmt.Errorf("remote sudo probe returned invalid path %q", boundedRemoteOutput([]byte(sudoPath))))
	}
	switch mode {
	case "noninteractive":
		return privilegeSpec{Mode: privilegeSudoNonInteractive, SudoPath: sudoPath}, nil
	case "password":
		return privilegeSpec{Mode: privilegeSudoPassword, SudoPath: sudoPath}, nil
	default:
		return privilegeSpec{}, diagnostic(DiagnosticSudoAuth, fmt.Errorf("remote sudo probe returned unsupported mode %q", boundedRemoteOutput([]byte(mode))))
	}
}

func (s privilegeSpec) acquire(ctx context.Context, remote remoteHost, target string, prompt SudoPasswordFunc) (privilege, error) {
	result := privilege{Spec: s}
	if s.Mode != privilegeSudoPassword {
		return result, nil
	}
	if prompt == nil {
		return privilege{}, diagnostic(DiagnosticSudoAuth, errors.New("remote sudo requires a password, but no interactive password prompt is available"))
	}
	provided, err := prompt(ctx, target)
	defer clear(provided)
	if err != nil {
		return privilege{}, diagnostic(DiagnosticSudoAuth, fmt.Errorf("read remote sudo password: %w", redactSecret(err, provided)))
	}
	if len(provided) == 0 {
		return privilege{}, diagnostic(DiagnosticSudoAuth, errors.New("remote sudo password is empty"))
	}
	if len(provided) > maximumSudoPasswordBytes {
		return privilege{}, diagnostic(DiagnosticSudoAuth, errors.New("remote sudo password is too large"))
	}
	if bytes.ContainsAny(provided, "\x00\r\n") {
		return privilege{}, diagnostic(DiagnosticSudoAuth, errors.New("remote sudo password contains a NUL byte or line break"))
	}
	result.Password = append([]byte(nil), provided...)
	if _, _, err := result.run(ctx, remote, "verify remote sudo password", "true", nil, true, DiagnosticSudoAuth); err != nil {
		clear(result.Password)
		return privilege{}, err
	}
	return result, nil
}

func (s privilegeSpec) command(command string) string {
	switch s.Mode {
	case privilegeRoot:
		return command
	case privilegeSudoNonInteractive:
		return shellQuote(s.SudoPath) + " -n " + command
	case privilegeSudoPassword:
		sudo := shellQuote(s.SudoPath)
		return "IFS= read -r mesh_sudo_password || exit 1; printf '%s\\n' \"$mesh_sudo_password\" | " + sudo + " -S -p '' -v && unset mesh_sudo_password && " + sudo + " -n " + command
	default:
		return command
	}
}

func (p privilege) run(ctx context.Context, remote remoteHost, operation, command string, payload []byte, sensitive bool, code DiagnosticCode, limits ...remoteOutputLimits) ([]byte, []byte, error) {
	command = p.Spec.command(command)
	stdin, disposable := p.stdin(payload)
	defer clear(disposable)
	stdout, stderr, err := remote.Run(ctx, command, stdin, limits...)
	if err == nil {
		if sensitive || p.Spec.Mode == privilegeSudoPassword {
			clear(stdout)
			clear(stderr)
			return nil, nil, nil
		}
		return stdout, stderr, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, diagnostic(code, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, diagnostic(code, err)
	}
	if sensitive || p.Spec.Mode == privilegeSudoPassword {
		clear(stdout)
		clear(stderr)
		return nil, nil, diagnostic(code, fmt.Errorf("%s failed; remote output was discarded because stdin contained a secret", operation))
	}
	return nil, nil, diagnostic(code, remoteCommandError(operation, err, stdout, stderr))
}

func (p privilege) stdin(payload []byte) (io.Reader, []byte) {
	if p.Spec.Mode != privilegeSudoPassword {
		if len(payload) == 0 {
			return nil, nil
		}
		return bytes.NewReader(payload), nil
	}
	contents := make([]byte, 0, len(p.Password)+1+len(payload))
	contents = append(contents, p.Password...)
	contents = append(contents, '\n')
	contents = append(contents, payload...)
	return bytes.NewReader(contents), contents
}

func (p *privilege) clear() {
	clear(p.Password)
	p.Password = nil
}
