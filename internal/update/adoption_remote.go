package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
)

type AdoptionLocalInput struct {
	Schema   int        `json:"schema"`
	ID       string     `json:"id"`
	Pin      string     `json:"pin"`
	Version  string     `json:"version"`
	Expected string     `json:"expected"`
	Policy   HostPolicy `json:"policy"`
	Require  []Host     `json:"require,omitempty"`
}

func DecodeAdoptionLocalInput(data []byte) (AdoptionLocalInput, error) {
	var input AdoptionLocalInput
	if len(data) == 0 || len(data) > 64<<10 {
		return input, errors.New("update: local adoption input exceeds 64 KiB or is empty")
	}
	if err := decodeAuthority(data, &input); err != nil {
		return input, err
	}
	if input.Schema != 1 || !ValidRunID(input.ID) || !hostDigest(input.Pin) {
		return input, errors.New("update: invalid local adoption operation or pin")
	}
	if _, err := PublicKey(input.Expected); err != nil {
		return input, err
	}
	if err := validateAdoptionPeers(input.Expected, input.Require); err != nil {
		return input, err
	}
	if input.Policy.Mode != HostAutomatic {
		return input, errors.New("update: local adoption requires explicit automatic policy")
	}
	return input, nil
}

func DeployAdoptionRemote(ctx context.Context, operation Adoption, remote AdoptionRemote) error {
	command, err := AdoptionRemoteCommand(operation, remote)
	if err != nil {
		return err
	}
	input := AdoptionLocalInput{Schema: 1, ID: operation.ID, Pin: operation.Pin, Version: operation.Version, Expected: remote.Host.ID, Policy: remote.Policy, Require: remote.Require}
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("update: encode remote adoption input: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, _, err = bootstrap.RunSystemSSH(ctx, remote.SSH, command, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("update: system SSH adoption for %s: %w", remote.Host.ID, err)
	}
	return nil
}

// AdoptionRemoteCommand has fixed download and staging steps. The only dynamic
// values are signed artifact names/digests and quoted operation identifiers.
func AdoptionRemoteCommand(operation Adoption, remote AdoptionRemote) (string, error) {
	if !ValidRunID(operation.ID) || !hostDigest(operation.Pin) {
		return "", errors.New("update: remote adoption requires an exact operation and pin")
	}
	if err := operation.Request.Manifest.Validate(); err != nil {
		return "", hostError(err)
	}
	if operation.Version != operation.Request.Manifest.Version {
		return "", errors.New("update: remote adoption release differs from pinned manifest")
	}
	if !completePlatform(remote.Host.Platform) {
		return "", fmt.Errorf("update: adoption remote %s has no recorded platform", remote.Host.ID)
	}
	artifact, err := operation.Request.Manifest.Artifact(remote.Host.Platform)
	if err != nil {
		return "", hostError(err)
	}
	format, err := artifact.Format()
	if err != nil || format != release.ArchiveGzip {
		return "", errors.New("update: adoption side image requires a gzip release archive")
	}
	address := release.OfficialBaseURL + "/download/" + operation.Version + "/" + artifact.Archive
	script := `set -eu
umask 077
` + adoptionCacheScript + `root="$cache/adoption"
mkdir -p "$root"
work=$(mktemp -d "$root/side.XXXXXXXX")
cleanup() { rm -f "$work/archive" "$work/mesh"; rmdir "$work"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 120 --max-filesize 134217728 --output "$work/archive" ` + adoptionShellQuote(address) + `
if command -v sha256sum >/dev/null 2>&1; then
  archive=$(sha256sum "$work/archive"); archive=${archive%% *}
else
  archive=$(shasum -a 256 "$work/archive"); archive=${archive%% *}
fi
test "$archive" = ` + adoptionShellQuote(artifact.SHA256) + ` || { printf '%s\n' 'adoption archive digest mismatch' >&2; exit 1; }
tar -xOzf "$work/archive" mesh > "$work/mesh"
if command -v sha256sum >/dev/null 2>&1; then
  image=$(sha256sum "$work/mesh"); image=${image%% *}
else
  image=$(shasum -a 256 "$work/mesh"); image=${image%% *}
fi
test "$image" = ` + adoptionShellQuote(artifact.BinarySHA256) + ` || { printf '%s\n' 'adoption executable digest mismatch' >&2; exit 1; }
chmod 700 "$work/mesh"
"$work/mesh" update adopt start --local --release ` + adoptionShellQuote(operation.Version) + ` --json
`
	return "sh -c " + adoptionShellQuote(script), nil
}

// adoptionCacheScript requires the daemon's existing update cache.
const adoptionCacheScript = paths.UpdateCacheShell + `test -d "$cache" || { printf 'existing daemon update cache %s is unavailable\n' "$cache" >&2; exit 1; }
`

// adoptionProbeScript prints the platform, then checks everything the deploy
// script needs before SSH can change anything, so a remote that would fail
// after SSH every time is refused before the adoption is recorded.
const adoptionProbeScript = `set -eu
uname -s
uname -m
` + adoptionCacheScript + `for tool in curl tar mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'adoption deploy needs %s on this host\n' "$tool" >&2; exit 1; }
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || { printf '%s\n' 'adoption deploy needs sha256sum or shasum on this host' >&2; exit 1; }
`

// RemoteAdoptionPlatform asks a fleet member for its platform, and checks the
// deploy's preconditions, over the same system SSH alias the deploy uses. It
// changes nothing on the remote.
func RemoteAdoptionPlatform(ctx context.Context, alias string) (release.Platform, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	platform, err := bootstrap.SystemSSHPlatform(ctx, alias, "sh -c "+adoptionShellQuote(adoptionProbeScript))
	if err != nil {
		return release.Platform{}, fmt.Errorf("update: system SSH platform for %s: %w", alias, err)
	}
	return release.Platform{OS: string(platform.OS), Arch: string(platform.Arch)}, nil
}

func completePlatform(platform release.Platform) bool {
	return platform.OS != "" && platform.Arch != ""
}

// resolveAdoptionPlatforms probes every remote before anything is recorded. The
// probe fills in a platform the fleet manifest left out, refuses one that
// differs from the host, and checks the deploy's preconditions; the pinned
// release must ship an artifact for each platform. Predecessor copies get the
// same platform so every recorded copy of a host agrees.
func resolveAdoptionPlatforms(ctx context.Context, start *AdoptionStart, manifest release.Manifest, detect func(context.Context, string) (release.Platform, error)) error {
	remotes := slices.Clone(start.Remotes)
	platforms := make(map[string]release.Platform, len(remotes))
	for i := range remotes {
		platform, err := resolveAdoptionPlatform(ctx, remotes[i], manifest, detect)
		if err != nil {
			return err
		}
		remotes[i].Host.Platform, platforms[remotes[i].Host.ID] = platform, platform
	}
	for i := range remotes {
		remotes[i].Require = withAdoptionPlatforms(remotes[i].Require, platforms)
	}
	start.Remotes, start.Require = remotes, withAdoptionPlatforms(start.Require, platforms)
	return nil
}

func resolveAdoptionPlatform(ctx context.Context, remote AdoptionRemote, manifest release.Manifest, detect func(context.Context, string) (release.Platform, error)) (release.Platform, error) {
	if detect == nil {
		return release.Platform{}, fmt.Errorf("update: adoption remote %s has no SSH platform probe", remote.Host.ID)
	}
	platform, err := detect(ctx, remote.SSH)
	if err != nil {
		return platform, fmt.Errorf("update: probe adoption remote %s over SSH %s: %w", remote.Host.ID, remote.SSH, err)
	}
	if !completePlatform(platform) {
		return platform, fmt.Errorf("update: adoption remote %s reported an incomplete platform", remote.Host.ID)
	}
	if completePlatform(remote.Host.Platform) && remote.Host.Platform != platform {
		return platform, fmt.Errorf("update: fleet records adoption remote %s as %s but the host reports %s", remote.Host.ID, remote.Host.Platform, platform)
	}
	if _, err := manifest.Artifact(platform); err != nil {
		return platform, fmt.Errorf("update: adoption remote %s: %w", remote.Host.ID, hostError(err))
	}
	return platform, nil
}

func withAdoptionPlatforms(hosts []Host, platforms map[string]release.Platform) []Host {
	if len(hosts) == 0 {
		return hosts
	}
	result := slices.Clone(hosts)
	for i, host := range result {
		if platform, ok := platforms[host.ID]; ok && !completePlatform(host.Platform) {
			result[i].Platform = platform
		}
	}
	return result
}

func adoptionShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
