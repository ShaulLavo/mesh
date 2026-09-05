package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
)

type Action string

const (
	ActionDefault Action = ""
	ActionShell   Action = "shell"
	ActionCommand Action = "command"
)

var ErrUncertain = errors.New("recovery: replacement launch is uncertain; retry to reconcile its reserved session")

// Source contains process facts supplied by the worker adapter, never inferred
// from a PID that another process could have reused.
type Source struct {
	ID, State, Cwd, BootID, RecoveredFrom, Shell string
	Command                                      []string
	Published                                    bool
}

type Runtime interface {
	Inspect(context.Context, string) (Source, error)
	Launch(context.Context, Launch) error
	ConfigureCommand(context.Context, string, *Command) error
}

type Config struct {
	SessionsDir string
	HostID      string
	BootID      string
	Runtime     Runtime
}

type Request struct {
	SessionID  string
	Action     Action
	Cols, Rows int
	Term       string
	Depth      int
}

type Launch struct {
	ID, SourceID string
	Command      []string
	Cwd          string
	Cols, Rows   int
	Term         string
	Depth        int
}

type Result struct {
	SessionID     string  `json:"sessionId,omitempty"`
	RecoveredFrom string  `json:"recoveredFrom,omitempty"`
	Cwd           string  `json:"cwd,omitempty"`
	OriginalCwd   string  `json:"originalCwd,omitempty"`
	Remote        *Target `json:"remote,omitempty"`
	Existing      bool    `json:"existing,omitempty"`
	State         string  `json:"state,omitempty"`
}

// LaunchFailure is returned only when Start proved that no worker was spawned.
// Every other launch error retains a dispatched reservation for reconciliation.
type LaunchFailure struct{ Err error }

func (e *LaunchFailure) Error() string { return e.Err.Error() }
func (e *LaunchFailure) Unwrap() error { return e.Err }

type intent struct {
	Version        int    `json:"version"`
	HostID         string `json:"hostId"`
	SourceID       string `json:"sourceId"`
	Action         Action `json:"action"`
	Phase          string `json:"phase"`
	Launch         Launch `json:"launch"`
	OriginalCwd    string `json:"originalCwd,omitempty"`
	DispatchBootID string `json:"dispatchBootId,omitempty"`
}

// Recover serializes all callers for a source, retains its checkpoint, and
// reconciles the exact reserved replacement after a dropped reply or crash.
func Recover(ctx context.Context, cfg Config, request Request) (Result, error) {
	if err := validateOperation(cfg, request.SessionID); err != nil {
		return Result{}, err
	}
	if request.Action != ActionDefault && request.Action != ActionShell && request.Action != ActionCommand {
		return Result{}, fmt.Errorf("recovery: invalid action %q", request.Action)
	}
	if !validField(request.Term) {
		return Result{}, errors.New("recovery: invalid terminal type")
	}
	dir := filepath.Join(cfg.SessionsDir, request.SessionID)
	unlock, err := lockSource(ctx, dir)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	previous, err := readIntent(dir, cfg.HostID, request.SessionID)
	if err == nil {
		return resumeIntent(ctx, cfg, dir, previous)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	source, err := cfg.Runtime.Inspect(ctx, request.SessionID)
	if err != nil {
		return Result{}, err
	}
	if source.State == "running" || source.State == "detached" {
		return Result{SessionID: source.ID, Cwd: source.Cwd, Existing: true, State: source.State}, nil
	}
	if !source.Published {
		return Result{}, fmt.Errorf("session %s: %w", request.SessionID, ErrUncertain)
	}
	record, err := ReadSaved(dir, cfg.HostID, request.SessionID, fallbackRecord(cfg.HostID, source))
	if err != nil && request.Action == ActionShell {
		record, err = fallbackRecord(cfg.HostID, source), nil
	}
	if err != nil {
		return Result{}, err
	}
	if record.Remote != nil && request.Action == ActionDefault {
		return Result{RecoveredFrom: source.ID, Remote: record.Remote}, nil
	}
	launch, originalCwd, err := chooseLaunch(source, record, request)
	if err != nil {
		return Result{}, err
	}
	id, err := reserveReplacement(cfg.SessionsDir)
	if err != nil {
		return Result{}, err
	}
	launch.ID = id
	next := intent{Version: 1, HostID: cfg.HostID, SourceID: source.ID, Action: request.Action, Phase: "reserved", Launch: launch, OriginalCwd: originalCwd}
	if err := writeTransaction(dir, "recovery-intent.json", next); err != nil {
		return Result{}, err
	}
	return resumeIntent(ctx, cfg, dir, next)
}

func resumeIntent(ctx context.Context, cfg Config, dir string, saved intent) (Result, error) {
	replacement, inspectErr := cfg.Runtime.Inspect(ctx, saved.Launch.ID)
	if inspectErr == nil && replacement.Published {
		if replacement.RecoveredFrom != saved.SourceID {
			return Result{}, fmt.Errorf("recovery: replacement %s belongs to a different source", saved.Launch.ID)
		}
		return completeIntent(dir, saved, true, replacement.State)
	}
	if inspectErr != nil && !errors.Is(inspectErr, os.ErrNotExist) {
		return Result{}, inspectErr
	}
	if saved.Phase == "dispatched" && saved.DispatchBootID != "" && cfg.BootID != "" && saved.DispatchBootID != cfg.BootID {
		saved.Phase = "reserved"
	}
	if saved.Phase != "reserved" {
		return Result{}, fmt.Errorf("session %s reserved replacement %s: %w", saved.SourceID, saved.Launch.ID, ErrUncertain)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	saved.Phase = "dispatched"
	saved.DispatchBootID = cfg.BootID
	if err := writeTransaction(dir, "recovery-intent.json", saved); err != nil {
		return Result{}, err
	}
	if err := cfg.Runtime.Launch(ctx, saved.Launch); err != nil {
		return Result{}, retainLaunchFailure(dir, saved, err)
	}
	replacement, err := cfg.Runtime.Inspect(ctx, saved.Launch.ID)
	if err != nil || !replacement.Published || replacement.RecoveredFrom != saved.SourceID {
		return Result{}, fmt.Errorf("session %s reserved replacement %s: %w", saved.SourceID, saved.Launch.ID, ErrUncertain)
	}
	return completeIntent(dir, saved, false, replacement.State)
}

func retainLaunchFailure(dir string, saved intent, err error) error {
	var failed *LaunchFailure
	if !errors.As(err, &failed) {
		return fmt.Errorf("session %s replacement %s: %w: %v", saved.SourceID, saved.Launch.ID, ErrUncertain, err)
	}
	saved.Phase = "reserved"
	if writeErr := writeTransaction(dir, "recovery-intent.json", saved); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return fmt.Errorf("session %s replacement %s: %w", saved.SourceID, saved.Launch.ID, err)
}

func completeIntent(dir string, saved intent, existing bool, state string) (Result, error) {
	if saved.Phase != "complete" {
		saved.Phase = "complete"
		if err := writeTransaction(dir, "recovery-intent.json", saved); err != nil {
			return Result{}, err
		}
	}
	return Result{SessionID: saved.Launch.ID, RecoveredFrom: saved.SourceID, Cwd: saved.Launch.Cwd, OriginalCwd: saved.OriginalCwd, Existing: existing, State: state}, nil
}

func chooseLaunch(source Source, record Record, request Request) (Launch, string, error) {
	launch := Launch{SourceID: source.ID, Cols: request.Cols, Rows: request.Rows, Term: request.Term, Depth: request.Depth}
	if request.Action == ActionCommand {
		command := Command{Argv: source.Command, Cwd: source.Cwd}
		if record.Restart != nil {
			command = *record.Restart
		}
		if err := ValidateCommand(&command); err != nil {
			return Launch{}, "", err
		}
		if err := requireDirectory(command.Cwd); err != nil {
			return Launch{}, "", err
		}
		launch.Command, launch.Cwd = append([]string(nil), command.Argv...), command.Cwd
		return launch, "", nil
	}
	shell := record.Shell
	if shell == "" {
		shell = source.Shell
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	launch.Command = []string{shell}
	cwd := record.ShellDirectory
	if cwd == "" {
		cwd = source.Cwd
	}
	actual, err := nearestDirectory(cwd)
	if err != nil {
		return Launch{}, "", err
	}
	launch.Cwd = actual
	if actual == cwd {
		return launch, "", nil
	}
	return launch, cwd, nil
}

func requireDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("recovery: working directory %q must be absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("recovery: working directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("recovery: working directory %s is not a directory", path)
	}
	return nil
}

func nearestDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("recovery: working directory %q must be absolute", path)
	}
	for {
		err := requireDirectory(path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

func fallbackRecord(hostID string, source Source) Record {
	return Record{Version: 1, HostID: hostID, SessionID: source.ID, Shell: source.Shell, ShellDirectory: source.Cwd, DirectorySource: DirectorySource("launch"), Command: source.Command}
}

// ReadSaved overlays an explicit recipe without giving another writer ownership
// of the worker's checkpoint. A missing checkpoint preserves legacy launch data.
func ReadSaved(dir, hostID, sessionID string, fallback Record) (Record, error) {
	record, err := Read(dir)
	if errors.Is(err, os.ErrNotExist) {
		record, err = fallback, nil
	}
	if err != nil {
		return Record{}, err
	}
	if record.HostID != hostID || record.SessionID != sessionID {
		return Record{}, fmt.Errorf("recovery: checkpoint owner does not match %s/%s", hostID, sessionID)
	}
	var recipe struct {
		Command *Command `json:"command"`
	}
	err = readTransaction(filepath.Join(dir, "recovery-command.json"), &recipe)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return Record{}, err
	}
	if recipe.Command != nil {
		if err := ValidateCommand(recipe.Command); err != nil {
			return Record{}, err
		}
	}
	record.Restart = recipe.Command
	return record, nil
}

func ConfigureCommand(ctx context.Context, cfg Config, id string, command *Command) error {
	if err := validateOperation(cfg, id); err != nil {
		return err
	}
	if command != nil {
		if err := ValidateCommand(command); err != nil {
			return err
		}
	}
	dir := filepath.Join(cfg.SessionsDir, id)
	unlock, err := lockSource(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	source, err := cfg.Runtime.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if source.State == "running" || source.State == "detached" {
		return cfg.Runtime.ConfigureCommand(ctx, id, command)
	}
	return writeTransaction(dir, "recovery-command.json", struct {
		Command *Command `json:"command"`
	}{command})
}

func ReplacementID(dir, hostID, id string) (string, error) {
	saved, err := readIntent(dir, hostID, id)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return saved.Launch.ID, nil
}

func validateOperation(cfg Config, id string) error {
	canonical, err := session.ParseID(id)
	if err != nil || canonical != id {
		return fmt.Errorf("recovery: invalid session ID %q", id)
	}
	if cfg.SessionsDir == "" || cfg.HostID == "" || cfg.Runtime == nil {
		return errors.New("recovery: incomplete host runtime configuration")
	}
	return nil
}

func readIntent(dir, hostID, id string) (intent, error) {
	var saved intent
	if err := readTransaction(filepath.Join(dir, "recovery-intent.json"), &saved); err != nil {
		return intent{}, err
	}
	if saved.Version != 1 || saved.HostID != hostID || saved.SourceID != id || saved.Launch.SourceID != id {
		return intent{}, fmt.Errorf("recovery: invalid recovery intent for %s/%s", hostID, id)
	}
	canonical, err := session.ParseID(saved.Launch.ID)
	if err != nil || canonical != saved.Launch.ID || canonical == id {
		return intent{}, fmt.Errorf("recovery: invalid replacement ID %q", saved.Launch.ID)
	}
	if saved.Phase != "reserved" && saved.Phase != "dispatched" && saved.Phase != "complete" {
		return intent{}, fmt.Errorf("recovery: invalid transaction phase %q", saved.Phase)
	}
	if err := ValidateCommand(&Command{Argv: saved.Launch.Command, Cwd: saved.Launch.Cwd}); err != nil {
		return intent{}, err
	}
	return saved, nil
}

func reserveReplacement(root string) (string, error) {
	for range 16 {
		id, err := session.NewID()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(root, id)
		err = os.Mkdir(dir, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("recovery: reserve replacement: %w", err)
		}
		if err := writeTransaction(dir, filepath.Base(paths.Launching(dir)), struct{}{}); err != nil {
			return "", err
		}
		if err := syncTransactionDirectory(root); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errors.New("recovery: exhausted replacement session IDs")
}

func lockSource(ctx context.Context, dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, "recovery.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed lock name inside a canonical host-local session directory
	if err != nil {
		return nil, fmt.Errorf("recovery: open source lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("recovery: lock source: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("recovery: wait for source lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func readTransaction(path string, value any) error {
	file, err := os.Open(path) //nolint:gosec // callers supply fixed transaction names inside a validated session directory
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // read-only handle; the read result carries the operation error
	data, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil {
		return fmt.Errorf("recovery: read transaction: %w", err)
	}
	if len(data) > 256<<10 {
		return errors.New("recovery: transaction exceeds size limit")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("recovery: decode transaction: %w", err)
	}
	return nil
}

func writeTransaction(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("recovery: encode transaction: %w", err)
	}
	if len(data) > 256<<10 {
		return errors.New("recovery: transaction exceeds size limit")
	}
	return atomicWrite(dir, name, data)
}

func syncTransactionDirectory(dir string) error {
	file, err := os.Open(dir) //nolint:gosec // sync the same trusted directory that owns the transaction
	if err != nil {
		return fmt.Errorf("recovery: open directory: %w", err)
	}
	defer file.Close() //nolint:errcheck // successful Sync is the durability boundary
	if err := file.Sync(); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("recovery: sync directory: %w", err)
	}
	return nil
}
