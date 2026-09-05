// Package recovery stores host-owned checkpoints and resolves explicit restarts.
package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/session"
)

const (
	Version         = 1
	MaxLines        = 256
	MaxTextBytes    = 128 << 10
	MaxRecordBytes  = 1 << 20
	MaxCommandBytes = 64 << 10
	MaxCommandArgs  = 256
	MaxFieldBytes   = 4 << 10
)

type DirectorySource string

const (
	DirectoryShell    DirectorySource = "shell"
	DirectoryObserved DirectorySource = "observed"
	DirectoryLaunch   DirectorySource = "launch"
)

type Command struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

type Target struct {
	HostID    string `json:"hostId"`
	SessionID string `json:"sessionId"`
}

type Record struct {
	Agent           *agentresume.Recipe  `json:"agent,omitempty"`
	AgentResume     *agentresume.Receipt `json:"agentResume,omitempty"`
	Version         int                  `json:"version"`
	HostID          string               `json:"hostId"`
	SessionID       string               `json:"sessionId"`
	CheckpointAt    time.Time            `json:"checkpointAt"`
	Shell           string               `json:"shell"`
	ShellDirectory  string               `json:"shellDirectory"`
	DirectorySource DirectorySource      `json:"directorySource"`
	Title           string               `json:"title,omitempty"`
	LastOutputAt    time.Time            `json:"lastOutputAt,omitempty"`
	Lines           []string             `json:"lines,omitempty"`
	Command         []string             `json:"command"`
	Restart         *Command             `json:"restart,omitempty"`
	Remote          *Target              `json:"remote,omitempty"`
}

func Validate(record Record) error {
	if record.Version != Version {
		return fmt.Errorf("recovery: unsupported record version %d", record.Version)
	}
	if err := validateTarget(Target{HostID: record.HostID, SessionID: record.SessionID}); err != nil {
		return fmt.Errorf("recovery: owner: %w", err)
	}
	if record.CheckpointAt.IsZero() {
		return fmt.Errorf("recovery: missing checkpoint time")
	}
	if !validField(record.Shell) || record.Shell == "" || !validDirectory(record.ShellDirectory) {
		return fmt.Errorf("recovery: invalid shell or shell directory")
	}
	switch record.DirectorySource {
	case DirectoryShell, DirectoryObserved, DirectoryLaunch:
	default:
		return fmt.Errorf("recovery: invalid directory source %q", record.DirectorySource)
	}
	if !validField(record.Title) {
		return fmt.Errorf("recovery: invalid title")
	}
	if err := validateArgv(record.Command); err != nil {
		return fmt.Errorf("recovery: launch command: %w", err)
	}
	if err := ValidateCommand(record.Restart); err != nil {
		return err
	}
	if err := validateAgentFields(record); err != nil {
		return err
	}
	if record.Remote != nil {
		if err := validateTarget(*record.Remote); err != nil {
			return fmt.Errorf("recovery: remote target: %w", err)
		}
	}
	return validateLines(record.Lines)
}

func validateAgentFields(record Record) error {
	if record.Agent != nil {
		if err := agentresume.ValidateRecipe(*record.Agent); err != nil {
			return fmt.Errorf("recovery: agent recipe: %w", err)
		}
	}
	if record.AgentResume != nil {
		if err := agentresume.ValidateReceipt(*record.AgentResume); err != nil {
			return fmt.Errorf("recovery: agent resume receipt: %w", err)
		}
	}
	return nil
}

func ValidateOwner(record Record, hostID, sessionID string) error {
	if err := Validate(record); err != nil {
		return err
	}
	if record.HostID != hostID || record.SessionID != sessionID {
		return fmt.Errorf("recovery: checkpoint belongs to %s/%s, expected %s/%s", record.HostID, record.SessionID, hostID, sessionID)
	}
	return nil
}

func ValidateCommand(command *Command) error {
	if command == nil {
		return nil
	}
	if !validDirectory(command.Cwd) {
		return fmt.Errorf("recovery: restart directory must be an absolute bounded path")
	}
	if err := validateArgv(command.Argv); err != nil {
		return fmt.Errorf("recovery: restart command: %w", err)
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxCommandArgs || argv[0] == "" {
		return fmt.Errorf("expected between 1 and %d arguments with an executable", MaxCommandArgs)
	}
	bytes := 0
	for _, arg := range argv {
		bytes += len(arg)
		if bytes > MaxCommandBytes || strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return fmt.Errorf("invalid arguments or more than %d argument bytes", MaxCommandBytes)
		}
	}
	return nil
}

func validateTarget(target Target) error {
	if target.HostID == "" || len(target.HostID) > 256 || strings.TrimSpace(target.HostID) != target.HostID || !validField(target.HostID) {
		return fmt.Errorf("invalid host identity")
	}
	if id, err := session.ParseID(target.SessionID); err != nil || id != target.SessionID {
		return fmt.Errorf("invalid session identity %q", target.SessionID)
	}
	return nil
}

func validateLines(lines []string) error {
	if len(lines) > MaxLines {
		return fmt.Errorf("recovery: more than %d output lines", MaxLines)
	}
	bytes := 0
	for _, line := range lines {
		bytes += len(line)
		if bytes > MaxTextBytes || !plainText(line) {
			return fmt.Errorf("recovery: invalid output text or more than %d bytes", MaxTextBytes)
		}
	}
	return nil
}

func validField(value string) bool {
	return len(value) <= MaxFieldBytes && plainText(value)
}

func validDirectory(value string) bool {
	return filepath.IsAbs(value) && validField(value)
}

func plainText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func Read(dir string) (Record, error) {
	var record Record
	file, err := os.Open(filepath.Join(dir, "recovery.json")) //nolint:gosec // fixed checkpoint name in the caller's private session directory
	if err != nil {
		return record, fmt.Errorf("recovery: open checkpoint: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only descriptor
	contents, err := io.ReadAll(io.LimitReader(file, MaxRecordBytes+1))
	if err != nil {
		return record, fmt.Errorf("recovery: read checkpoint: %w", err)
	}
	if len(contents) > MaxRecordBytes {
		return record, fmt.Errorf("recovery: checkpoint exceeds %d bytes", MaxRecordBytes)
	}
	if err := json.Unmarshal(contents, &record); err != nil {
		return Record{}, fmt.Errorf("recovery: decode checkpoint: %w", err)
	}
	if err := Validate(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func Write(dir string, record Record) error {
	if err := Validate(record); err != nil {
		return err
	}
	contents, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("recovery: encode checkpoint: %w", err)
	}
	if len(contents) > MaxRecordBytes {
		return fmt.Errorf("recovery: encoded checkpoint exceeds %d bytes", MaxRecordBytes)
	}
	return atomicWrite(dir, "recovery.json", append(contents, '\n'))
}

func atomicWrite(dir, name string, contents []byte) error {
	temporary := filepath.Join(dir, name+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // caller owns this private session directory
	if err != nil {
		return fmt.Errorf("recovery: create temporary %s: %w", name, err)
	}
	defer os.Remove(temporary) //nolint:errcheck // cleanup after failure or successful rename
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("recovery: write %s: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("recovery: sync %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("recovery: close %s: %w", name, err)
	}
	if err := os.Rename(temporary, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("recovery: publish %s: %w", name, err)
	}
	directory, err := os.Open(dir) //nolint:gosec // sync the private directory containing the published checkpoint
	if err != nil {
		return fmt.Errorf("recovery: open directory for sync: %w", err)
	}
	defer directory.Close() //nolint:errcheck // directory Sync determines durability
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("recovery: sync directory: %w", err)
	}
	return nil
}
