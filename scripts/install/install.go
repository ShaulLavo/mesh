// Package install embeds the remote installers and canonical service assets.
package install

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

var (
	//go:embed linux.sh
	linux string
	//go:embed darwin.sh
	darwin string
	//go:embed assets/mesh.service
	systemdService string
	//go:embed assets/dev.shaulavo.mesh.plist
	launchdService string
)

// ServiceOptions provides the configurable values in a service asset.
type ServiceOptions struct {
	DaemonPort    uint16
	WebSocketPath string
}

// Script returns the installer for a Go operating-system name.
func Script(goos string) (string, bool) {
	switch goos {
	case "linux":
		return linux, true
	case "darwin":
		return darwin, true
	default:
		return "", false
	}
}

func serviceAsset(goos string) (string, bool) {
	switch goos {
	case "linux":
		return systemdService, true
	case "darwin":
		return launchdService, true
	default:
		return "", false
	}
}

// RenderService renders the canonical service asset for one remote install.
func RenderService(goos string, opts ServiceOptions) (string, error) {
	asset, ok := serviceAsset(goos)
	if !ok {
		return "", fmt.Errorf("unsupported service operating system %q", goos)
	}
	if opts.DaemonPort == 0 {
		return "", fmt.Errorf("service port must be positive")
	}
	if opts.WebSocketPath == "" || opts.WebSocketPath[0] != '/' {
		return "", fmt.Errorf("service WebSocket path must be absolute")
	}
	for _, r := range opts.WebSocketPath {
		if !(r == '/' || r == '-' || r == '_' || r == '.' || r == '~' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "", fmt.Errorf("service WebSocket path contains unsupported character %q", r)
		}
	}
	binaryPath := "%h/.local/bin/mesh"
	standardOut := ""
	standardErr := ""
	if goos == "darwin" {
		binaryPath = "${HOME}/.local/bin/mesh"
		standardOut = "${HOME}/.local/state/mesh/daemon.log"
		standardErr = "${HOME}/.local/state/mesh/daemon.err.log"
	}
	asset = strings.NewReplacer(
		"@MESH_BINARY@", binaryPath,
		"@MESH_PORT@", strconv.Itoa(int(opts.DaemonPort)),
		"@MESH_WEBSOCKET_PATH@", opts.WebSocketPath,
		"@MESH_STDOUT@", standardOut,
		"@MESH_STDERR@", standardErr,
	).Replace(asset)
	if strings.Contains(asset, "@MESH_") {
		return "", fmt.Errorf("service asset for %s contains an unresolved token", goos)
	}
	return asset, nil
}
