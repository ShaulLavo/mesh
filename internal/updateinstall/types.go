// Package updateinstall owns durable, per-installation update transactions.
package updateinstall

import (
	"context"
	"errors"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type Phase string

const (
	Accepted       Phase = "accepted"
	Staged         Phase = "staged"
	Granted        Phase = "granted"
	Activating     Phase = "activating"
	Validating     Phase = "validating"
	Committed      Phase = "committed"
	RollingBack    Phase = "rolling_back"
	RolledBack     Phase = "rolled_back"
	RollbackFailed Phase = "rollback_failed"
	Cancelled      Phase = "cancelled"
	Failed         Phase = "failed"
)

var (
	ErrConflict            = errors.New("another update owns this installation")
	ErrStaleGeneration     = errors.New("stale update generation")
	ErrNoGrant             = errors.New("update is staged and awaiting an activation grant")
	ErrAlreadyGranted      = errors.New("activation already authorized; cancellation cannot revoke it")
	ErrInstallationChanged = errors.New("installation changed outside its update transaction")
)

type Service interface {
	Stop(context.Context) error
	Start(context.Context) error
}

type Worker struct {
	ID       string         `json:"id"`
	PID      int            `json:"pid"`
	ShellPID int            `json:"shellPid,omitempty"`
	Protocol int            `json:"protocol"`
	Build    *release.Build `json:"build,omitempty"`
}

type Health struct {
	HostID             string        `json:"hostId"`
	BootID             string        `json:"bootId,omitempty"`
	Build              release.Build `json:"build"`
	Workers            []Worker      `json:"workers"`
	InterruptedWorkers []Worker      `json:"interruptedWorkers,omitempty"`
}

type Probe func(context.Context) (Health, error)

type ServiceSpec struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Domain     string `json:"domain,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
}

type Config struct {
	StateDir      string
	Executable    string
	CacheDir      string
	RequiredMount string
	ClientOnly    bool
	HealthTimeout time.Duration
	ServiceSpec   ServiceSpec
	Client        release.Client
	Service       Service
	Probe         Probe
}

type Request struct {
	ID         string           `json:"id"`
	TargetID   string           `json:"targetId"`
	Generation uint64           `json:"generation"`
	Manifest   release.Manifest `json:"manifest"`
	Current    release.Build    `json:"current"`
}

type Settings struct {
	StateDir      string        `json:"stateDir"`
	Executable    string        `json:"executable"`
	CacheDir      string        `json:"cacheDir"`
	RequiredMount string        `json:"requiredMount,omitempty"`
	ClientOnly    bool          `json:"clientOnly"`
	HealthTimeout time.Duration `json:"healthTimeout"`
	Service       ServiceSpec   `json:"service"`
}

func (s Settings) Config() Config {
	return Config{StateDir: s.StateDir, Executable: s.Executable, CacheDir: s.CacheDir,
		RequiredMount: s.RequiredMount, ClientOnly: s.ClientOnly, HealthTimeout: s.HealthTimeout, ServiceSpec: s.Service}
}

type Status struct {
	RetryToken uint64    `json:"retryToken,omitempty"`
	Schema     int       `json:"schema"`
	Phase      Phase     `json:"phase"`
	Request    Request   `json:"request"`
	Settings   Settings  `json:"settings"`
	Candidate  string    `json:"candidate"`
	Previous   string    `json:"previous"`
	Original   Health    `json:"original"`
	Verified   *Health   `json:"verified,omitempty"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Engine struct{ cfg Config }
