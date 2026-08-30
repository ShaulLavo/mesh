package bootstrap

import (
	"fmt"
	"strings"
)

func parsePlatform(output []byte) (Platform, error) {
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return Platform{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("uname returned %q, want OS and architecture", strings.TrimSpace(string(output))))
	}
	var platform Platform
	switch fields[0] {
	case "Linux":
		platform.OS = Linux
	case "Darwin":
		platform.OS = Darwin
	default:
		return Platform{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("unsupported operating system %q", fields[0]))
	}
	switch fields[1] {
	case "x86_64", "amd64":
		platform.Arch = AMD64
	case "aarch64", "arm64":
		platform.Arch = ARM64
	default:
		return Platform{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("unsupported architecture %q on %s", fields[1], platform.OS))
	}
	return platform, nil
}
