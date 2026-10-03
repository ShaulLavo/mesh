package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

func remoteAccount(ctx context.Context, remote remoteHost) (string, error) {
	stdout, stderr, err := remote.Run(ctx, "id -u; id -un", nil)
	if err != nil {
		return "", remoteCommandError("read destination daemon account", err, stdout, stderr)
	}
	lines := strings.Fields(string(stdout))
	if len(lines) != 2 {
		return "", errors.New("bootstrap: destination did not report its OS account")
	}
	if lines[0] == "0" {
		return "root", nil
	}
	return lines[1], nil
}

func trustedDestinationIdentity(ctx context.Context, remote remoteHost) (string, error) {
	stdout, stderr, err := remote.Run(ctx, `"$HOME/.local/bin/mesh" device identity --json`, nil)
	if err != nil {
		return "", remoteCommandError("read destination Mesh identity over SSH", err, stdout, stderr)
	}
	var observation struct {
		ID          string `json:"id"`
		Account     string `json:"account"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(stdout, &observation); err != nil {
		return "", fmt.Errorf("bootstrap: decode trusted destination identity: %w", err)
	}
	if _, err := identity.IdentityKey(observation.ID); err != nil {
		return "", fmt.Errorf("bootstrap: trusted destination key: %w", err)
	}
	return observation.ID, nil
}

func reportDeviceApproval(progress func(Event), source, destination string) error {
	for _, device := range []struct{ label, id string }{{"source", source}, {"destination", destination}} {
		key, err := identity.IdentityKey(device.id)
		if err != nil {
			return fmt.Errorf("bootstrap: decode approval fingerprint: %w", err)
		}
		public, err := ssh.NewPublicKey(key)
		if err != nil {
			return fmt.Errorf("bootstrap: encode approval fingerprint: %w", err)
		}
		progress(Event{Step: StepVerify, Detail: device.label + " key " + ssh.FingerprintSHA256(public)})
	}
	return nil
}
