package updatebootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/worker"
	_ "modernc.org/sqlite"
)

type Observation struct {
	Executable string
	Health     updateinstall.Health
}

type executableImage struct {
	Path, Installed string
	PID             int
}

func Probe(stateDir string) updateinstall.Probe {
	return func(ctx context.Context) (updateinstall.Health, error) {
		observed, err := Inspect(ctx, stateDir)
		return observed.Health, err
	}
}

func Inspect(ctx context.Context, stateDir string) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, filepath.Join(stateDir, "daemon.sock"))
	if err != nil {
		return Observation{}, err
	}
	defer func() { _ = conn.Close() }()
	image, err := peerImage(conn)
	if err != nil {
		return Observation{}, err
	}
	image.Installed = activeInstallationPath(stateDir, image)
	response, err := exchange(conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "bootstrap-host-info"})
	if err != nil {
		return Observation{}, err
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		return Observation{}, errors.New("legacy daemon did not return host identity")
	}
	stateVersion, err := readStateVersion(ctx, stateDir)
	if err != nil {
		return Observation{}, err
	}
	build, err := observedBuild(image.Path, response.Host.Build, stateVersion)
	if err != nil {
		return Observation{}, err
	}
	if response.Host.Build == nil {
		build.WorkerProtocol = 1
	}
	health := updateinstall.Health{HostID: response.Host.ID, BootID: worker.BootID(), Build: build}
	health.Workers, err = inspectWorkers(ctx, stateDir, stateVersion)
	if err != nil {
		return Observation{}, err
	}
	return Observation{Executable: image.Installed, Health: health}, nil
}

func readStateVersion(ctx context.Context, stateDir string) (int, error) {
	path := filepath.Join(stateDir, "mesh.db")
	if _, err := os.Stat(path); err != nil { //nolint:gosec // read-only inspection of the caller-selected local Mesh state directory
		return 0, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var version int
	err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied = 1").Scan(&version)
	return version, err
}

func observedBuild(path string, reported *release.Build, stateVersion int) (release.Build, error) {
	digest, err := imageDigest(path)
	if err != nil {
		return release.Build{}, err
	}
	if reported != nil {
		if reported.Digest != digest {
			return release.Build{}, errors.New("reported build differs from the connected process executable")
		}
		build := *reported
		build.StateVersion = stateVersion
		return build, nil
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return release.Build{Digest: digest, Platform: release.CurrentPlatform(), StateVersion: stateVersion, Modified: true}, nil
	}
	build := release.Build{Digest: digest, Platform: release.CurrentPlatform(), StateVersion: stateVersion}
	build.Version = info.Main.Version
	for _, setting := range info.Settings {
		applyLegacySetting(&build, setting.Key, setting.Value)
	}
	if _, err := release.CompareVersions(build.Version, "v0.0.0"); err != nil {
		build.Version = legacyVersionFlag(path)
	}
	return build, nil
}

func legacyVersionFlag(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output() //nolint:gosec // only the OS-identified, already-running local executable is queried
	if err != nil || len(output) > 4096 {
		return ""
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "mesh version ")
	if _, err = release.CompareVersions(version, "v0.0.0"); err != nil {
		return ""
	}
	return version
}

func applyLegacySetting(build *release.Build, key, value string) {
	if key == "vcs.revision" {
		build.Commit = value
		return
	}
	if key == "vcs.modified" {
		build.Modified = value == "true"
		return
	}
	if key != "-ldflags" {
		return
	}
	for _, field := range strings.Fields(value) {
		if strings.HasPrefix(field, "github.com/shaul/mesh/internal/bootstrap.releaseVersion=") {
			build.Version = strings.TrimPrefix(field, "github.com/shaul/mesh/internal/bootstrap.releaseVersion=")
		}
	}
}

func imageDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // OS-reported executable of the connected local process
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, (128<<20)+1))
	if err != nil {
		return "", err
	}
	if size > 128<<20 {
		return "", errors.New("legacy executable exceeds supported size")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func inspectWorkers(ctx context.Context, stateDir string, stateVersion int) ([]updateinstall.Worker, error) {
	root := filepath.Join(stateDir, "s")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var workers []updateinstall.Worker
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		current, err := inspectWorker(ctx, filepath.Join(root, entry.Name()), stateVersion)
		if err != nil {
			return nil, err
		}
		if current != nil {
			workers = append(workers, *current)
		}
	}
	return workers, nil
}

func inspectWorker(ctx context.Context, dir string, stateVersion int) (*updateinstall.Worker, error) {
	if _, err := os.Lstat(paths.Launching(dir)); err == nil { //nolint:gosec // dir comes from entries enumerated under the local workers directory
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	meta, err := worker.ReadMeta(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if meta.State == worker.StateExited || meta.State == worker.StateInterrupted {
		return nil, nil
	}
	if meta.BootID != "" && meta.BootID != worker.BootID() {
		return nil, nil
	}
	conn, err := dial(ctx, filepath.Join(dir, "sock"))
	if err != nil {
		return nil, fmt.Errorf("verify session %s: %w", meta.ID, err)
	}
	defer func() { _ = conn.Close() }()
	image, err := peerImage(conn)
	if err != nil {
		return nil, err
	}
	response, err := exchange(conn, protocol.Control{Type: protocol.TypeInspect, RequestID: "bootstrap-worker", PreviewCols: 1, PreviewRows: 1})
	if err != nil {
		return nil, err
	}
	if !validWorkerProbe(response, meta) {
		return nil, errors.New("worker cannot answer a read-only session inspection")
	}
	build, err := observedBuild(image.Path, meta.Build, stateVersion)
	if err != nil {
		return nil, err
	}
	if meta.Build == nil {
		build.WorkerProtocol = 1
	}
	return &updateinstall.Worker{ID: meta.ID, PID: image.PID, ShellPID: meta.PID, Protocol: build.WorkerProtocol, Build: &build}, nil
}

func validWorkerProbe(response protocol.Control, meta worker.Meta) bool {
	if response.Type == protocol.TypeInspected {
		return response.Inspection != nil && response.SessionID == meta.ID && response.RequestID == "bootstrap-worker"
	}
	// Workers before session.inspect reject the unknown request before the
	// attachment path. This exact framed response is their read-only liveness
	// acknowledgement; peerImage has already bound it to the executable and PID.
	return meta.Build == nil && response.Type == protocol.TypeError && response.Inspection == nil &&
		response.SessionID == meta.ID && response.RequestID == "" && response.Message == "expected "+protocol.TypeAttach
}

func dial(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func exchange(conn net.Conn, request protocol.Control) (protocol.Control, error) {
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		return protocol.Control{}, err
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("unexpected legacy response frame")
	}
	return protocol.DecodeControl(frame.Payload)
}
