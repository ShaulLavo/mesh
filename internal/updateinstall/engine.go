package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
)

var operationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)

func New(cfg Config) (*Engine, error) {
	if !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.Executable) || !filepath.IsAbs(cfg.CacheDir) {
		return nil, errors.New("update state, executable, and cache paths must be absolute")
	}
	if cfg.RequiredMount != "" && !within(cfg.RequiredMount, cfg.CacheDir) {
		return nil, errors.New("cache must be within the required data mount")
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 30 * time.Second
	}
	if cfg.HealthTimeout > 5*time.Minute {
		return nil, errors.New("update health timeout exceeds five minutes")
	}
	if !cfg.ClientOnly && cfg.Probe == nil {
		return nil, errors.New("daemon updates require an executing-build and worker health probe")
	}
	if cfg.Service == nil && !cfg.ClientOnly {
		service, err := NewSystemService(cfg.ServiceSpec)
		if err != nil {
			return nil, err
		}
		cfg.Service = service
	}
	return &Engine{cfg: cfg}, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (e *Engine) settings() Settings {
	return Settings{StateDir: e.cfg.StateDir, Executable: e.cfg.Executable, CacheDir: e.cfg.CacheDir,
		RequiredMount: e.cfg.RequiredMount, ClientOnly: e.cfg.ClientOnly, HealthTimeout: e.cfg.HealthTimeout, Service: e.cfg.ServiceSpec}
}

func (e *Engine) Stage(ctx context.Context, request Request) (Status, error) {
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock(lock)
	status, err := e.accept(ctx, request)
	if err != nil || status.Phase != Accepted {
		return status, err
	}
	return e.stage(ctx, status)
}

func (e *Engine) accept(ctx context.Context, request Request) (Status, error) {
	if err := validateRequest(request); err != nil {
		return Status{}, err
	}
	prior, err := e.Read()
	if err == nil {
		if sameRequest(prior, request) {
			return prior, nil
		}
		if !finished(prior.Phase) {
			return prior, ErrConflict
		}
		if request.Generation <= prior.Request.Generation {
			return prior, ErrStaleGeneration
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return prior, err
	}
	manifest, err := e.cfg.Client.Manifest(ctx, request.Manifest.Version)
	if err != nil {
		return Status{}, err
	}
	if manifest.Digest() != request.Manifest.Digest() {
		return Status{}, errors.New("approved manifest does not match official release")
	}
	health, err := e.preflight(ctx, request)
	if err != nil {
		return Status{}, err
	}
	dir := filepath.Dir(e.cfg.Executable)
	status := Status{Schema: 1, Phase: Accepted, Request: request, Settings: e.settings(), Original: health,
		Candidate: filepath.Join(dir, ".mesh-update-"+request.ID+".candidate"), Previous: filepath.Join(dir, ".mesh-update-"+request.ID+".previous")}
	return status, e.save(&status)
}

func validateRequest(request Request) error {
	if !operationPattern.MatchString(request.ID) || request.TargetID == "" || request.Generation == 0 {
		return errors.New("invalid update operation, target identity, or generation")
	}
	if err := request.Manifest.Validate(); err != nil {
		return err
	}
	if request.Current.Platform != release.CurrentPlatform() {
		return errors.New("approved platform does not match this host")
	}
	if request.Manifest.Compatibility.JournalVersion != 1 {
		return errors.New("release requires an unsupported installation journal format")
	}
	if err := request.Manifest.Allows(request.Current); err != nil {
		return err
	}
	order, err := release.CompareVersions(request.Manifest.Version, request.Current.Version)
	if err != nil {
		// Allows already required the exact source-to-target receipt; an unknown
		// legacy version cannot supply an additional semantic ordering check.
		return nil
	}
	if order <= 0 {
		return errors.New("installation updates require a strictly newer release")
	}
	return nil
}

func sameRequest(status Status, request Request) bool {
	return status.Request.TargetID == request.TargetID && status.Request.Generation == request.Generation &&
		status.Request.Manifest.Digest() == request.Manifest.Digest()
}

func finished(phase Phase) bool {
	return phase == Committed || phase == RolledBack || phase == Cancelled || phase == Failed
}

func (e *Engine) preflight(ctx context.Context, request Request) (Health, error) {
	if service, ok := e.cfg.Service.(interface{ Preflight(context.Context) error }); ok && !e.cfg.ClientOnly {
		if err := service.Preflight(ctx); err != nil {
			return Health{}, err
		}
	}
	if err := ValidateInstallationPath(e.cfg.Executable); err != nil {
		return Health{}, err
	}
	if err := checkMount(e.cfg.RequiredMount); err != nil {
		return Health{}, err
	}
	if err := verifyFile(e.cfg.Executable, request.Current.Digest); err != nil {
		return Health{}, err
	}
	info, err := os.Lstat(e.cfg.Executable)
	if err != nil {
		return Health{}, err
	}
	size := info.Size()
	if size < 0 || size > 128<<20 {
		return Health{}, errors.New("installed executable exceeds supported size")
	}
	if err = os.MkdirAll(e.cfg.CacheDir, 0700); err != nil {
		return Health{}, err
	}
	if err = checkSpace(e.cfg.CacheDir, 128<<20); err != nil {
		return Health{}, err
	}
	if err = checkSpace(filepath.Dir(e.cfg.Executable), uint64(size)*2+(16<<20)); err != nil {
		return Health{}, err
	}
	if e.cfg.ClientOnly {
		return Health{HostID: request.TargetID, Build: request.Current}, nil
	}
	health, err := e.cfg.Probe(ctx)
	if err != nil {
		return Health{}, err
	}
	if health.HostID != request.TargetID || health.Build.Digest != request.Current.Digest {
		return Health{}, errors.New("running daemon identity or build differs from approved installation")
	}
	if err = request.Manifest.Allows(health.Build); err != nil {
		return Health{}, err
	}
	if err = compatibleWorkers(health, request.Manifest.Compatibility); err != nil {
		return Health{}, err
	}
	return health, nil
}

func compatibleWorkers(health Health, compatibility release.Compatibility) error {
	for _, worker := range health.Workers {
		if worker.Protocol < compatibility.WorkerMin || worker.Protocol > compatibility.WorkerMax {
			return fmt.Errorf("session %s has an incompatible or unknown worker protocol", worker.ID)
		}
	}
	return nil
}

func (e *Engine) stage(ctx context.Context, status Status) (Status, error) {
	if err := checkMount(e.cfg.RequiredMount); err != nil {
		return status, err
	}
	path, err := e.cfg.Client.Download(ctx, status.Request.Manifest, status.Request.Current.Platform, e.cfg.CacheDir)
	if err != nil {
		return status, err
	}
	artifact, err := status.Request.Manifest.Artifact(status.Request.Current.Platform)
	if err != nil {
		return status, err
	}
	if err = durableCopy(path, status.Candidate, artifact.BinarySHA256); err != nil {
		return status, err
	}
	if err = durableCopy(e.cfg.Executable, status.Previous, status.Request.Current.Digest); err != nil {
		return status, err
	}
	status.Phase = Staged
	return status, e.save(&status)
}

func (e *Engine) Grant(ctx context.Context, id string, generation uint64) (Status, error) {
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock(lock)
	status, err := e.identify(id, generation)
	if err != nil {
		return status, err
	}
	if status.Phase == Cancelled {
		return status, ErrStaleGeneration
	}
	if status.Phase == RolledBack || status.Phase == RollingBack || status.Phase == RollbackFailed || status.Phase == Failed {
		return status, errors.New("failed activation requires explicit retry before a new grant")
	}
	if status.Phase != Staged && status.Phase != Accepted {
		return status, nil
	}
	if status.Phase == Accepted {
		return status, errors.New("release must finish staging before activation is authorized")
	}
	status.Phase = Granted
	return status, e.save(&status)
}

func (e *Engine) identify(id string, generation uint64) (Status, error) {
	status, err := e.Read()
	if err != nil {
		return status, err
	}
	if status.Request.Generation != generation {
		return status, ErrStaleGeneration
	}
	if status.Request.ID != id {
		return status, ErrConflict
	}
	return status, nil
}

func (e *Engine) Cancel(ctx context.Context, id string, generation uint64) (Status, error) {
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock(lock)
	status, err := e.identify(id, generation)
	if err != nil || status.Phase == Cancelled {
		return status, err
	}
	if status.Phase != Accepted && status.Phase != Staged {
		return status, ErrAlreadyGranted
	}
	status.Phase = Cancelled
	return status, e.save(&status)
}

func (e *Engine) Retry(ctx context.Context, id string, generation uint64) (Status, error) {
	return e.RetryWithToken(ctx, id, generation, 0)
}

// RetryWithToken makes retransmission of one explicitly approved retry idempotent.
// A zero token allocates the next token for an ordinary local retry command.
func (e *Engine) RetryWithToken(ctx context.Context, id string, generation, token uint64) (Status, error) {
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock(lock)
	status, err := e.identify(id, generation)
	if err != nil {
		return status, err
	}
	if token != 0 && token <= status.RetryToken {
		return status, nil
	}
	if status.Phase != RolledBack && status.Phase != Failed {
		return status, errors.New("only a verified rollback or pre-activation failure can be retried")
	}
	health, err := e.preflight(ctx, status.Request)
	if err != nil {
		return status, err
	}
	health.InterruptedWorkers = interruptedWorkers(status.Original, health)
	status.Original, status.Verified, status.Error, status.Phase = health, nil, "", Accepted
	if token == 0 {
		token = status.RetryToken + 1
	}
	status.RetryToken = token
	if err = e.save(&status); err != nil {
		return status, err
	}
	return e.stage(ctx, status)
}
