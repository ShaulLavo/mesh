package bootstrap

import (
	"context"
	"encoding/base64"
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
	WebSocketPath string
}

func installRemote(ctx context.Context, remote remoteHost, request installRequest) (bool, error) {
	script, ok := installscript.Script(request.Platform.OS.String())
	if !ok {
		return false, diagnostic(DiagnosticWrongArch, fmt.Errorf("no installer for %s", request.Platform.OS))
	}
	service, err := installscript.RenderService(request.Platform.OS.String(), installscript.ServiceOptions{
		DaemonPort:    request.DaemonPort,
		WebSocketPath: request.WebSocketPath,
	})
	if err != nil {
		return false, diagnostic(DiagnosticServiceInstall, fmt.Errorf("render remote Mesh service: %w", err))
	}
	stdout, stderr, err := remote.Run(ctx, `umask 077; mktemp "${TMPDIR:-/tmp}/mesh-bootstrap.XXXXXX"`, nil)
	if err != nil {
		return false, diagnostic(DiagnosticServiceInstall, remoteCommandError("create remote upload file", err, stdout, stderr))
	}
	remoteBinary := strings.TrimSpace(string(stdout))
	if err := validateRemoteTemporaryPath(remoteBinary); err != nil {
		return false, diagnostic(DiagnosticServiceInstall, err)
	}
	defer cleanupRemoteTemporary(remote, remoteBinary)

	binary, err := os.Open(request.BinaryPath)
	if err != nil {
		return false, diagnostic(DiagnosticWrongArch, fmt.Errorf("open Mesh binary %s: %w", request.BinaryPath, err))
	}
	uploadErr := uploadBinary(ctx, remote, remoteBinary, binary)
	closeErr := binary.Close()
	if uploadErr != nil {
		return false, uploadErr
	}
	if closeErr != nil {
		return false, diagnostic(DiagnosticServiceInstall, fmt.Errorf("close Mesh binary %s: %w", request.BinaryPath, closeErr))
	}

	authorizedKey := base64.StdEncoding.EncodeToString([]byte(request.AuthorizedKey))
	serviceAsset := base64.StdEncoding.EncodeToString([]byte(service))
	command := strings.Join([]string{
		"/bin/sh -s --",
		shellQuote(remoteBinary),
		shellQuote(strconv.Itoa(int(request.DaemonPort))),
		shellQuote(request.WebSocketPath),
		shellQuote(authorizedKey),
		shellQuote(serviceAsset),
	}, " ")
	stdout, stderr, err = remote.Run(ctx, command, strings.NewReader(script))
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
