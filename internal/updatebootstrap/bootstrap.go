// Package updatebootstrap installs the first updater through a legacy Mesh session.
package updatebootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type Request struct {
	RetryToken    uint64           `json:"retryToken,omitempty"`
	ID            string           `json:"id"`
	TargetID      string           `json:"targetId"`
	CoordinatorID string           `json:"coordinatorId"`
	Generation    uint64           `json:"generation"`
	Manifest      release.Manifest `json:"manifest"`
}

type Config struct {
	StateDir      string
	CacheDir      string
	RequiredMount string
	Client        release.Client
	Enroll        func(stateDir, coordinatorID string) error
}

var operationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)

func (r Request) Validate() error {
	if !operationID.MatchString(r.ID) || r.Generation == 0 {
		return errors.New("invalid bootstrap operation or generation")
	}
	for _, id := range []string{r.TargetID, r.CoordinatorID} {
		key, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return errors.New("bootstrap requires pinned target and coordinator identities")
		}
	}
	return r.Manifest.Validate()
}

func Decode(encoded string) (Request, error) {
	var request Request
	if len(encoded) > 256<<10 {
		return request, errors.New("bootstrap request exceeds size limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return request, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		return request, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return request, errors.New("trailing bootstrap request data")
	}
	return request, request.Validate()
}

func Command(request Request) ([]string, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	args := []string{"/bin/sh", "-c", downloadScript, "mesh-update-bootstrap", request.ID, request.Manifest.Version, base64.RawURLEncoding.EncodeToString(data)}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		artifact, err := request.Manifest.Artifact(platform)
		if err != nil {
			return nil, err
		}
		args = append(args, artifact.BinarySHA256)
	}
	return args, nil
}

// StatusCommand retrieves the durable target receipt through a legacy session.
// It never downloads artifacts, grants activation, or reruns an installer.
func StatusCommand(request Request) ([]string, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", statusScript, "mesh-update-bootstrap-status", request.ID, fmt.Sprint(request.Generation), request.Manifest.Digest(), request.TargetID}, nil
}

func ReadStatus(stateDir, id string, generation uint64, digest, target string) (updateinstall.Status, error) {
	if !operationID.MatchString(id) || generation == 0 || len(digest) != 64 {
		return updateinstall.Status{}, errors.New("invalid bootstrap status request")
	}
	status, err := updateinstall.Read(stateDir)
	if err != nil {
		return status, err
	}
	if status.Request.ID != id || status.Request.Generation != generation || status.Request.Manifest.Digest() != digest || status.Request.TargetID != target {
		return status, errors.New("installation receipt does not match the requested bootstrap")
	}
	return status, nil
}

// Run records an approved legacy operation and hands activation to the supervised
// helper. The attaching session never owns daemon replacement or rollback.
func Run(ctx context.Context, request Request, cfg Config) (updateinstall.Status, error) {
	if err := request.Validate(); err != nil {
		return updateinstall.Status{}, err
	}
	if cfg.Enroll == nil {
		return updateinstall.Status{}, errors.New("bootstrap requires an owner-controlled enrollment adapter")
	}
	manifest, err := cfg.Client.Manifest(ctx, request.Manifest.Version)
	if err != nil {
		return updateinstall.Status{}, err
	}
	if manifest.Digest() != request.Manifest.Digest() {
		return updateinstall.Status{}, errors.New("bootstrap release differs from the exact approved manifest")
	}
	prior, priorErr := existing(request, cfg.StateDir)
	if priorErr != nil && !errors.Is(priorErr, os.ErrNotExist) {
		return prior, priorErr
	}
	if priorErr == nil && prior.Phase != updateinstall.Staged && prior.Phase != updateinstall.Accepted && request.RetryToken == 0 {
		return prior, nil
	}
	if priorErr == nil && request.RetryToken != 0 {
		return resumeRetry(ctx, request, cfg, prior)
	}
	observed, err := Inspect(ctx, cfg.StateDir)
	if err != nil {
		return updateinstall.Status{}, err
	}
	if observed.Health.HostID != request.TargetID {
		return updateinstall.Status{}, errors.New("bootstrap target identity changed")
	}
	if err = manifest.Allows(observed.Health.Build); err != nil {
		return updateinstall.Status{}, err
	}
	if err = updateinstall.ValidateInstallationPath(observed.Executable); err != nil {
		return updateinstall.Status{}, err
	}
	if err = persistRequest(cfg.StateDir, request); err != nil {
		return updateinstall.Status{}, err
	}
	spec, err := updateinstall.DefaultServiceSpec()
	if err != nil {
		return updateinstall.Status{}, err
	}
	engine, err := updateinstall.New(updateinstall.Config{StateDir: cfg.StateDir, Executable: observed.Executable, CacheDir: cfg.CacheDir, RequiredMount: cfg.RequiredMount, ServiceSpec: spec, Client: cfg.Client, Probe: Probe(cfg.StateDir)})
	if err != nil {
		return updateinstall.Status{}, err
	}
	if err = cfg.Enroll(cfg.StateDir, request.CoordinatorID); err != nil {
		return updateinstall.Status{}, err
	}
	if err = installBootstrapHelper(ctx, cfg.StateDir, spec); err != nil {
		return updateinstall.Status{}, err
	}
	status, err := engine.Stage(ctx, updateinstall.Request{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, Manifest: manifest, Current: observed.Health.Build})
	if err != nil {
		return status, err
	}
	return engine.Grant(ctx, status.Request.ID, status.Request.Generation)
}

func installBootstrapHelper(ctx context.Context, stateDir string, spec updateinstall.ServiceSpec) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	_, err = updateinstall.InstallHelper(ctx, updateinstall.HelperConfig{StateDir: stateDir, Executable: executable, Kind: spec.Kind, Domain: spec.Domain})
	return err
}

func resumeRetry(ctx context.Context, request Request, cfg Config, prior updateinstall.Status) (updateinstall.Status, error) {
	config := prior.Settings.Config()
	config.Client = cfg.Client
	config.Probe = Probe(cfg.StateDir)
	engine, err := updateinstall.New(config)
	if err != nil {
		return prior, err
	}
	status, err := engine.RetryWithToken(ctx, request.ID, request.Generation, request.RetryToken)
	if err != nil {
		return status, err
	}
	if status.Phase == updateinstall.Accepted {
		status, err = engine.Stage(ctx, status.Request)
	}
	if err != nil || status.Phase != updateinstall.Staged {
		return status, err
	}
	if err = cfg.Enroll(cfg.StateDir, request.CoordinatorID); err != nil {
		return status, err
	}
	executable, err := os.Executable()
	if err != nil {
		return status, err
	}
	_, err = updateinstall.InstallHelper(ctx, updateinstall.HelperConfig{StateDir: cfg.StateDir, Executable: executable, Kind: config.ServiceSpec.Kind, Domain: config.ServiceSpec.Domain})
	if err != nil {
		return status, err
	}
	return engine.Grant(ctx, status.Request.ID, status.Request.Generation)
}

func existing(request Request, stateDir string) (updateinstall.Status, error) {
	status, err := updateinstall.Read(stateDir)
	if err != nil {
		return status, err
	}
	if status.Request.ID != request.ID || status.Request.TargetID != request.TargetID || status.Request.Generation != request.Generation || status.Request.Manifest.Digest() != request.Manifest.Digest() {
		return status, errors.New("bootstrap journal belongs to another operation")
	}
	return status, nil
}

func persistRequest(stateDir string, request Request) error {
	path := filepath.Join(stateDir, "update", "bootstrap.request.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".bootstrap-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path)) //nolint:gosec // fixed bootstrap journal directory
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("persist bootstrap approval: %w", err)
	}
	return nil
}

const downloadScript = `set -eu
umask 077
operation=$1
version=$2
request=$3
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) platform=linux_amd64; expected=$4 ;;
  Linux/aarch64|Linux/arm64) platform=linux_arm64; expected=$5 ;;
  Darwin/arm64) platform=darwin_arm64; expected=$6 ;;
  *) printf '%s\n' 'Mesh bootstrap: unsupported platform' >&2; exit 1 ;;
esac
if [ -n "${MESH_UPDATE_REQUIRED_MOUNT:-}" ]; then
  [ -d "$MESH_UPDATE_REQUIRED_MOUNT" ] || exit 1
  mounted=$(df -P "$MESH_UPDATE_REQUIRED_MOUNT" | awk 'END {print $NF}')
  [ "$mounted" = "$MESH_UPDATE_REQUIRED_MOUNT" ] || { printf '%s\n' 'Mesh bootstrap: required data mount missing' >&2; exit 1; }
fi
cache=${MESH_UPDATE_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/mesh/updates}
mkdir -p "$cache"
temporary=$(mktemp -d "$cache/bootstrap-$operation.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
archive=mesh_$platform.tar.gz
origin=https://github.com/ShaulLavo/mesh/releases/download/$version
curl --fail --location --proto '=https' --proto-redir '=https' --max-time 180 --max-filesize 134217728 "$origin/$archive" -o "$temporary/$archive"
curl --fail --location --proto '=https' --proto-redir '=https' --max-time 30 --max-filesize 1048576 "$origin/checksums.txt" -o "$temporary/checksums.txt"
expected_archive=$(awk -v name="$archive" '$2==name {print $1}' "$temporary/checksums.txt")
[ "${#expected_archive}" = 64 ] || exit 1
case "$expected_archive" in *[!0-9a-f]*) exit 1 ;; esac
hash() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi; }
actual=$(hash "$temporary/$archive"); actual=${actual%% *}
[ "$actual" = "$expected_archive" ] || { printf '%s\n' 'Mesh bootstrap: official archive checksum mismatch' >&2; exit 1; }
entries=$(tar -tzf "$temporary/$archive")
[ "$entries" = mesh ] || { printf '%s\n' 'Mesh bootstrap: unexpected archive contents' >&2; exit 1; }
(ulimit -f 262144; tar -xzOf "$temporary/$archive" mesh > "$temporary/mesh")
[ -f "$temporary/mesh" ] && [ ! -L "$temporary/mesh" ] || exit 1
[ "$(wc -c < "$temporary/mesh")" -le 134217728 ] || exit 1
actual=$(hash "$temporary/mesh"); actual=${actual%% *}
[ "$actual" = "$expected" ] || { printf '%s\n' 'Mesh bootstrap: approved executable checksum mismatch' >&2; exit 1; }
chmod 700 "$temporary/mesh"
"$temporary/mesh" update-bootstrap --request-base64 "$request"
`

const statusScript = `set -eu
state=${MESH_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/mesh}
if [ ! -f "$state/update/installation.json" ]; then
  printf '%s\n' 'MESH_UPDATE_RECEIPT_UNAVAILABLE' >&2
  exit 3
fi
helper=$state/update/helper/current
if [ -x "$helper" ]; then
  printf 'MESH_UPDATE_RECEIPT='
  exec "$helper" update-bootstrap-status --state-dir "$state" --id "$1" --generation "$2" --manifest-digest "$3" --target "$4"
fi
printf 'MESH_UPDATE_RECEIPT='
head -c 1048576 "$state/update/installation.json"
`
