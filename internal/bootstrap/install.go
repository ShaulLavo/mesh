package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	installscript "github.com/shaul/mesh/scripts/install"
)

type installRequest struct {
	Platform      Platform
	BinaryPath    string
	AuthorizedKey string
	DaemonPort    uint16
	SSHPort       uint16
	WebSocketPath string
	Progress      func(Event)
}

func (r installRequest) progress(event Event) {
	if r.Progress == nil {
		return
	}
	r.Progress(event)
}

func installRemote(ctx context.Context, remote remoteHost, request installRequest) (bool, error) {
	script, ok := installscript.Script(request.Platform.OS.String())
	if !ok {
		return false, diagnostic(DiagnosticWrongArch, fmt.Errorf("no installer for %s", request.Platform.OS))
	}
	service, err := installscript.RenderService(request.Platform.OS.String(), installscript.ServiceOptions{
		DaemonPort:    request.DaemonPort,
		SSHPort:       request.SSHPort,
		WebSocketPath: request.WebSocketPath,
	})
	if err != nil {
		return false, diagnostic(DiagnosticServiceInstall, fmt.Errorf("render remote Mesh service: %w", err))
	}
	remoteBinary, cleanup, err := stageBinary(ctx, remote, request)
	if err != nil {
		return false, err
	}
	defer cleanup()

	authorizedKey := base64.StdEncoding.EncodeToString([]byte(request.AuthorizedKey))
	serviceAsset := base64.StdEncoding.EncodeToString([]byte(service))
	command := strings.Join([]string{
		"/bin/sh -s --",
		shellQuote(remoteBinary),
		shellQuote(strconv.Itoa(int(request.DaemonPort))),
		shellQuote(strconv.Itoa(int(request.SSHPort))),
		shellQuote(request.WebSocketPath),
		shellQuote(authorizedKey),
		shellQuote(serviceAsset),
	}, " ")
	stdout, stderr, err := remote.Run(ctx, command, strings.NewReader(script))
	combined := string(stdout) + "\n" + string(stderr)
	if err != nil {
		if code, ok := installerDiagnostic(combined); ok {
			return false, diagnostic(code, remoteCommandError("install remote Mesh service", err, stdout, stderr))
		}
		return false, diagnostic(DiagnosticServiceInstall, remoteCommandError("install remote Mesh service", err, stdout, stderr))
	}
	switch {
	case containsLine(stdout, "MESH_INSTALL_RESULT=unchanged"):
		return true, nil
	case containsLine(stdout, "MESH_INSTALL_RESULT=configured"):
		return false, nil
	default:
		return false, diagnostic(DiagnosticServiceInstall, fmt.Errorf("installer returned no result marker: %s", strings.TrimSpace(combined)))
	}
}

// stageBinary uploads the binary unless the host already runs exactly these
// bytes. It returns the empty path when it skipped, which tells the installer
// to keep the binary it has and reconcile only the service and the keys.
func stageBinary(ctx context.Context, remote remoteHost, request installRequest) (string, func(), error) {
	if remoteBinaryIsCurrent(ctx, remote, request.BinaryPath) {
		request.progress(Event{Step: StepTransfer, Detail: "host already runs this build"})
		return "", func() {}, nil
	}
	stdout, stderr, err := remote.Run(ctx, `umask 077; mktemp "${TMPDIR:-/tmp}/mesh-bootstrap.XXXXXX"`, nil)
	if err != nil {
		return "", nil, diagnostic(DiagnosticServiceInstall, remoteCommandError("create remote upload file", err, stdout, stderr))
	}
	remoteBinary := strings.TrimSpace(string(stdout))
	if err := validateRemoteTemporaryPath(remoteBinary); err != nil {
		return "", nil, diagnostic(DiagnosticServiceInstall, err)
	}
	cleanup := func() { cleanupRemoteTemporary(remote, remoteBinary) }

	binary, err := os.Open(request.BinaryPath)
	if err != nil {
		cleanup()
		return "", nil, diagnostic(DiagnosticWrongArch, fmt.Errorf("open Mesh binary %s: %w", request.BinaryPath, err))
	}
	uploadErr := uploadBinary(ctx, remote, remoteBinary, binary)
	closeErr := binary.Close()
	if uploadErr != nil {
		cleanup()
		return "", nil, uploadErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, diagnostic(DiagnosticServiceInstall, fmt.Errorf("close Mesh binary %s: %w", request.BinaryPath, closeErr))
	}
	return remoteBinary, cleanup, nil
}

// installedBinaryProbe prints the digest of the binary the service runs. It
// prints nothing when there is no binary or no hasher, and silence means
// upload: an unknown digest must never be read as a match.
const installedBinaryProbe = `binary="$HOME/.local/bin/mesh"
[ -x "$binary" ] || exit 0
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum -- "$binary" | cut -d " " -f 1
elif command -v shasum >/dev/null 2>&1; then
	shasum -a 256 -- "$binary" | cut -d " " -f 1
fi`

func remoteBinaryIsCurrent(ctx context.Context, remote remoteHost, localPath string) bool {
	stdout, _, err := remote.Run(ctx, installedBinaryProbe, nil)
	if err != nil {
		return false
	}
	installed := strings.TrimSpace(string(stdout))
	if len(installed) != hex.EncodedLen(sha256.Size) {
		return false
	}
	staged, err := fileDigest(localPath)
	if err != nil {
		return false
	}
	return strings.EqualFold(installed, staged)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func uploadBinary(ctx context.Context, remote remoteHost, remotePath string, binary io.Reader) error {
	command := "umask 077; cat > " + shellQuote(remotePath) + " && chmod 0700 " + shellQuote(remotePath)
	stdout, stderr, err := remote.Run(ctx, command, binary)
	if err != nil {
		return diagnostic(DiagnosticServiceInstall, remoteCommandError("upload Mesh binary", err, stdout, stderr))
	}
	return nil
}

func validateRemoteTemporaryPath(value string) error {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || !strings.HasPrefix(filepath.Base(value), "mesh-bootstrap.") {
		return fmt.Errorf("mktemp returned unsafe path %q", value)
	}
	for _, r := range value {
		if !(r == '/' || r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("mktemp returned unsafe path %q", value)
		}
	}
	return nil
}

func cleanupRemoteTemporary(remote remoteHost, remotePath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = remote.Run(ctx, "rm -f -- "+shellQuote(remotePath), nil)
}

func installerDiagnostic(output string) (DiagnosticCode, bool) {
	for _, line := range strings.Split(output, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "MESH_BOOTSTRAP_ERROR=")
		if !found {
			continue
		}
		switch DiagnosticCode(value) {
		case DiagnosticNoSystemd, DiagnosticNoUserLingering, DiagnosticServiceInstall:
			return DiagnosticCode(value), true
		default:
			return DiagnosticServiceInstall, true
		}
	}
	return "", false
}

func containsLine(output []byte, want string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
