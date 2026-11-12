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
	"sync"
	"sync/atomic"
	"syscall"
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
	PID        int
}

type executableImage struct {
	Path, Installed string
	PID             int
}

type processImageResult struct {
	image executableImage
	err   error
}

var workerImages = processImages

func peerImage(ctx context.Context, conn net.Conn) (executableImage, error) {
	pid, err := peerPID(conn)
	if err != nil {
		return executableImage{}, err
	}
	return processImage(ctx, pid)
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
		return Observation{}, fmt.Errorf("verify daemon phase=dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	image, err := peerImage(ctx, conn)
	if err != nil {
		return Observation{}, fmt.Errorf("verify daemon phase=peer-image: %w", err)
	}
	image.Installed = activeInstallationPath(stateDir, image)
	response, err := exchange(conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "bootstrap-host-info"})
	if err != nil {
		return Observation{}, fmt.Errorf("verify daemon PID=%d: %w", image.PID, err)
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		return Observation{}, errors.New("legacy daemon did not return host identity")
	}
	stateVersion, err := readStateVersion(ctx, stateDir)
	if err != nil {
		return Observation{}, err
	}
	build, err := observedBuild(ctx, image.Path, response.Host.Build, stateVersion)
	if err != nil {
		return Observation{}, fmt.Errorf("verify daemon PID=%d phase=build-identity: %w", image.PID, err)
	}
	if response.Host.Build == nil {
		build.WorkerProtocol = 1
	}
	health := updateinstall.Health{HostID: response.Host.ID, BootID: worker.BootID(), Build: build}
	health.Workers, err = inspectWorkers(ctx, stateDir, stateVersion)
	if err != nil {
		return Observation{}, err
	}
	return Observation{Executable: image.Installed, Health: health, PID: image.PID}, nil
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

func observedBuild(ctx context.Context, path string, reported *release.Build, stateVersion int) (release.Build, error) {
	digest, err := imageDigestContext(ctx, path)
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
		build.Version = legacyVersionFlag(ctx, path)
	}
	if err := ctx.Err(); err != nil {
		return release.Build{}, fmt.Errorf("inspect legacy build: %w", err)
	}
	return build, nil
}

func legacyVersionFlag(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version") //nolint:gosec // only the OS-identified, already-running local executable is queried
	command.WaitDelay = 100 * time.Millisecond
	output, err := command.Output()
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

func imageDigestContext(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("inspect executable digest: %w", err)
	}
	file, err := os.Open(path) //nolint:gosec // OS-reported executable of the connected local process
	if err != nil {
		return "", fmt.Errorf("open executable image: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(probeImageReader{ctx: ctx, file: file}, (128<<20)+1))
	if err != nil {
		return "", fmt.Errorf("hash executable image: %w", err)
	}
	if size > 128<<20 {
		return "", errors.New("legacy executable exceeds supported size")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("inspect executable digest: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type probeImageReader struct {
	ctx  context.Context
	file *os.File
}

func (r probeImageReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read executable image: %w", err)
	}
	n, err := r.file.Read(buffer)
	if err == io.EOF {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read executable image: %w", err)
	}
	return n, nil
}

type workerObservation struct {
	dir    string
	meta   worker.Meta
	conn   net.Conn
	pid    int
	image  executableImage
	worker *updateinstall.Worker
	err    error
	done   bool
}

const workerProbeConcurrency = 8
const workerImageBatch = 32

func inspectWorkers(ctx context.Context, stateDir string, stateVersion int) ([]updateinstall.Worker, error) {
	root := filepath.Join(stateDir, "s")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return collectWorkerObservations(ctx, nil)
	}
	if err != nil {
		return nil, err
	}
	var observations []workerObservation
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		observations = append(observations, workerObservation{dir: filepath.Join(root, entry.Name())})
	}
	for start := 0; start < len(observations) && ctx.Err() == nil; start += workerImageBatch {
		inspectWorkerGroup(ctx, observations[start:min(start+workerImageBatch, len(observations))], stateVersion)
	}
	return collectWorkerObservations(ctx, observations)
}

func inspectWorkerGroup(ctx context.Context, observations []workerObservation, stateVersion int) {
	// Own connections before Connect can acquire one, including entries never asked.
	defer closeWorkerConnections(observations)
	runWorkerStage(ctx, observations, func(current *workerObservation) { current.connect(ctx) })
	if ctx.Err() != nil {
		return
	}
	identifyWorkers(ctx, observations)
	runWorkerStage(ctx, observations, func(current *workerObservation) { current.ask() })
	runWorkerStage(ctx, observations, func(current *workerObservation) { current.digest(ctx, stateVersion) })
}

func closeWorkerConnections(observations []workerObservation) {
	for i := range observations {
		if observations[i].conn != nil {
			_ = observations[i].conn.Close()
		}
	}
}

func runWorkerStage(ctx context.Context, observations []workerObservation, stage func(*workerObservation)) {
	var next atomic.Int64
	var group sync.WaitGroup
	for range min(workerProbeConcurrency, len(observations)) {
		group.Go(func() { runWorkerStageBatch(ctx, observations, stage, &next) })
	}
	group.Wait()
}

func runWorkerStageBatch(ctx context.Context, observations []workerObservation, stage func(*workerObservation), next *atomic.Int64) {
	for {
		index := int(next.Add(1)) - 1
		if index >= len(observations) || ctx.Err() != nil {
			return
		}
		current := &observations[index]
		if current.done || current.err != nil {
			continue
		}
		stage(current)
	}
}

func identifyWorkers(ctx context.Context, observations []workerObservation) {
	var pids []int
	indices := make(map[int]int)
	for i := range observations {
		current := &observations[i]
		if current.done || current.err != nil {
			continue
		}
		if _, exists := indices[current.pid]; exists {
			continue
		}
		indices[current.pid] = len(pids)
		pids = append(pids, current.pid)
	}
	if len(pids) == 0 || ctx.Err() != nil {
		return
	}
	results := workerImages(ctx, pids)
	for i := range observations {
		current := &observations[i]
		if current.done || current.err != nil {
			continue
		}
		result := results[indices[current.pid]]
		current.image = result.image
		if result.err != nil {
			current.err = fmt.Errorf("verify session %s shellPID=%d phase=peer-image: %w", current.meta.ID, current.meta.PID, result.err)
		}
	}
}

func collectWorkerObservations(ctx context.Context, observations []workerObservation) ([]updateinstall.Worker, error) {
	var workers []updateinstall.Worker
	for _, current := range observations {
		if current.err != nil {
			return nil, errors.Join(ctx.Err(), current.err)
		}
		if current.worker != nil {
			workers = append(workers, *current.worker)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("inspect worker cohort: %w", err)
	}
	return workers, nil
}

func (current *workerObservation) connect(ctx context.Context) {
	dir := current.dir
	if _, err := os.Lstat(paths.Launching(dir)); err == nil { //nolint:gosec // dir comes from entries enumerated under the local workers directory
		current.done = true
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		current.err = fmt.Errorf("inspect worker metadata: %w", err)
		return
	}
	meta, err := worker.ReadMeta(dir)
	if errors.Is(err, os.ErrNotExist) {
		current.done = true
		return
	}
	if err != nil {
		current.err = fmt.Errorf("inspect worker metadata: %w", err)
		return
	}
	if meta.State == worker.StateExited || meta.State == worker.StateInterrupted {
		current.done = true
		return
	}
	if meta.BootID != "" && meta.BootID != worker.BootID() {
		current.done = true
		return
	}
	conn, err := dial(ctx, filepath.Join(dir, "sock"))
	// A killed worker (SIGKILL, systemd-oomd) never records its end, so its
	// metadata still says running. Nothing listening means nothing to preserve.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		current.done = true
		return
	}
	if err != nil {
		current.err = fmt.Errorf("verify session %s shellPID=%d phase=dial: %w", meta.ID, meta.PID, err)
		return
	}
	current.conn = conn
	current.meta = meta
	current.pid, err = peerPID(conn)
	if err != nil {
		current.err = fmt.Errorf("verify session %s shellPID=%d phase=peer-image: %w", meta.ID, meta.PID, err)
	}
}

func (current *workerObservation) ask() {
	defer func() { _ = current.conn.Close() }()
	meta, image := current.meta, current.image
	response, err := exchange(current.conn, protocol.Control{Type: protocol.TypeInspect, RequestID: "bootstrap-worker", PreviewCols: 1, PreviewRows: 1})
	if err != nil {
		current.err = fmt.Errorf("verify session %s workerPID=%d shellPID=%d: %w", meta.ID, image.PID, meta.PID, err)
		return
	}
	if !validWorkerProbe(response, meta) {
		current.err = errors.New("worker cannot answer a read-only session inspection")
	}
}

func (current *workerObservation) digest(ctx context.Context, stateVersion int) {
	meta, image := current.meta, current.image
	build, err := observedBuild(ctx, image.Path, meta.Build, stateVersion)
	if err != nil {
		current.err = fmt.Errorf("verify session %s workerPID=%d shellPID=%d phase=build-identity: %w", meta.ID, image.PID, meta.PID, err)
		return
	}
	if meta.Build == nil {
		build.WorkerProtocol = 1
	}
	current.worker = &updateinstall.Worker{ID: meta.ID, PID: image.PID, ShellPID: meta.PID, Protocol: build.WorkerProtocol, Build: &build}
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
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(closed)
	})
	return &probeConnection{Conn: conn, stop: stop, closed: closed}, nil
}

type probeConnection struct {
	net.Conn
	stop   func() bool
	closed <-chan struct{}
	once   sync.Once
	err    error
}

func (c *probeConnection) Close() error {
	c.once.Do(func() {
		if !c.stop() {
			<-c.closed
		}
		c.err = c.Conn.Close()
	})
	return c.err
}

func (c *probeConnection) SyscallConn() (syscall.RawConn, error) {
	socket, ok := c.Conn.(syscall.Conn)
	if !ok {
		return nil, errors.New("bootstrap requires a local Unix socket")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("inspect local socket: %w", err)
	}
	return raw, nil
}

func exchange(conn net.Conn, request protocol.Control) (protocol.Control, error) {
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		return protocol.Control{}, fmt.Errorf("phase=write-request: %w", err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("phase=read-response: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("unexpected legacy response frame")
	}
	return protocol.DecodeControl(frame.Payload)
}
