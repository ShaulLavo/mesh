package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

type AdoptionPhase string

const (
	AdoptionRecorded   AdoptionPhase = "recorded"
	AdoptionWaiting    AdoptionPhase = "waiting"
	AdoptionActivating AdoptionPhase = "activating"
	AdoptionConfirming AdoptionPhase = "confirming"
	AdoptionComplete   AdoptionPhase = "adopted"
	AdoptionRolledBack AdoptionPhase = "rolled-back"
	AdoptionAttention  AdoptionPhase = "attention"
)

// Adoption is stored only in HostAdoption.Details. The envelope owns confirmation
// and milestones so a helper restart cannot create a second source of authority.
type Adoption struct {
	Schema      int                               `json:"schema"`
	ID          string                            `json:"id"`
	Pin         string                            `json:"pin"`
	Signature   string                            `json:"signature"`
	Version     string                            `json:"version"`
	Host        AdoptionHost                      `json:"host"`
	Policy      HostPolicy                        `json:"policy"`
	Require     []Host                            `json:"require,omitempty"`
	Notice      *AdoptionNotice                   `json:"notice,omitempty"`
	Remotes     []AdoptionRemote                  `json:"remotes,omitempty"`
	Phase       AdoptionPhase                     `json:"phase"`
	Request     updateinstall.Request             `json:"request"`
	Settings    updateinstall.Settings            `json:"settings"`
	Helper      *updateinstall.HelperInstallation `json:"helper,omitempty"`
	Problem     string                            `json:"problem,omitempty"`
	NextAttempt time.Time                         `json:"nextAttempt,omitempty"`
	Failures    uint32                            `json:"failures,omitempty"`
}

type AdoptionHost struct {
	Health    updateinstall.Health `json:"health"`
	Preserved map[string]string    `json:"preserved"`
	Service   string               `json:"service"`
}

type AdoptionNotice struct {
	Command     []string      `json:"command"`
	Lead        time.Duration `json:"lead"`
	DeliveredAt time.Time     `json:"deliveredAt,omitempty"`
}

type AdoptionRemote struct {
	Host        Host       `json:"host"`
	SSH         string     `json:"ssh"`
	Policy      HostPolicy `json:"policy"`
	Require     []Host     `json:"require,omitempty"`
	State       string     `json:"state"`
	Problem     string     `json:"problem,omitempty"`
	NextAttempt time.Time  `json:"nextAttempt,omitempty"`
}

func (a Adoption) validate() error {
	if a.Schema != 1 || !ValidRunID(a.ID) || !hostDigest(a.Pin) || !hostDigest(a.Signature) {
		return errors.New("update: invalid adoption identity or document pin")
	}
	if err := validateHostPolicy(a.Policy); err != nil {
		return err
	}
	if !validAdoptionPhase(a.Phase) || a.Settings.ClientOnly || a.Settings.StateDir == "" || !hostDigest(a.Host.Service) {
		return errors.New("update: invalid adoption phase or daemon installation")
	}
	if a.Request.ID != a.ID || a.Request.TargetID != a.Host.Health.HostID || a.Request.Generation == 0 || a.Request.Automatic != nil || !sameHostIdentity(a.Request.Current, a.Host.Health.Build) || a.Version != a.Request.Manifest.Version {
		return errors.New("update: adoption request differs from observed installation")
	}
	if err := validateAdoptionPeers(a.Host.Health.HostID, a.Require); err != nil {
		return err
	}
	if a.Notice != nil && (len(a.Notice.Command) == 0 || a.Notice.Command[0] == "" || a.Notice.Lead < 0) {
		return errors.New("update: adoption notice requires an executable and nonnegative lead time")
	}
	if len(a.Remotes) > MaximumHosts {
		return errors.New("update: too many adoption remotes")
	}
	for i, remote := range a.Remotes {
		if err := validateAdoptionRemote(remote, a.Host.Health.HostID, a.Remotes[:i]); err != nil {
			return err
		}
	}
	return nil
}

func validAdoptionPhase(phase AdoptionPhase) bool {
	return phase == AdoptionRecorded || phase == AdoptionWaiting || phase == AdoptionActivating || phase == AdoptionConfirming || phase == AdoptionComplete || phase == AdoptionRolledBack || phase == AdoptionAttention
}

func validateAdoptionPeers(local string, peers []Host) error {
	if len(peers) > MaximumHosts {
		return errors.New("update: too many adoption predecessors")
	}
	seen := make(map[string]bool, len(peers))
	for _, host := range peers {
		if err := host.Validate(); err != nil {
			return err
		}
		if host.ID == local || seen[host.ID] {
			return errors.New("update: duplicate or local adoption predecessor")
		}
		seen[host.ID] = true
	}
	return nil
}

func validateAdoptionRemote(remote AdoptionRemote, local string, previous []AdoptionRemote) error {
	if err := remote.Host.Validate(); err != nil {
		return err
	}
	if remote.Host.ID == local || remote.SSH == "" {
		return errors.New("update: adoption remote requires a distinct host and configured system SSH alias")
	}
	for _, prior := range previous {
		if prior.Host.ID == remote.Host.ID {
			return errors.New("update: duplicate adoption remote")
		}
	}
	return validateAdoptionPeers(remote.Host.ID, remote.Require)
}

func (s *Store) adoption() (HostAdoption, Adoption, error) {
	var envelope HostAdoption
	if err := s.readAuthority("host-adoption.json", &envelope); err != nil {
		return envelope, Adoption{}, err
	}
	if err := envelope.validate(); err != nil {
		return envelope, Adoption{}, err
	}
	var operation Adoption
	if err := decodeAuthority(envelope.Details, &operation); err != nil {
		return envelope, operation, err
	}
	if operation.ID != envelope.Operation || operation.Pin != envelope.Pin {
		return envelope, operation, errors.New("update: adoption details differ from canonical envelope")
	}
	return envelope, operation, operation.validate()
}

func (s *Store) AdoptionStatus(ctx context.Context) (HostAdoption, Adoption, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostAdoption{}, Adoption{}, err
	}
	defer unlock()
	return s.adoption()
}

func (s *Store) lockAdoptionPass(ctx context.Context) (func(), error) {
	dir, err := s.authorityDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), "adoption.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("update: open adoption pass lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), "adoption.lock")
	if err := privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	bound, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := waitAuthority(bound, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

func encodeAdoption(envelope *HostAdoption, operation Adoption) error {
	if err := operation.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(operation)
	if err != nil {
		return fmt.Errorf("update: encode adoption: %w", err)
	}
	envelope.Details = data
	return envelope.validate()
}

func (s *Store) beginAdoption(ctx context.Context, operation Adoption, expectedPolicy uint64) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	_, prior, err := s.adoption()
	if err == nil {
		if prior.ID == operation.ID && prior.Pin == operation.Pin {
			return nil
		}
		if err := canReplaceAdoption(prior, operation.Version); err != nil {
			return err
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	policy, err := s.hostPolicy()
	if err != nil {
		return err
	}
	if policy.Revision != expectedPolicy || expectedPolicy == ^uint64(0) || operation.Policy.Revision != expectedPolicy+1 {
		return errors.New("update: host policy changed before adoption")
	}
	envelope := HostAdoption{Schema: 1, Operation: operation.ID, Pin: operation.Pin}
	if err := encodeAdoption(&envelope, operation); err != nil {
		return err
	}
	// Publish the hold first. A crash before policy publication keeps the updater
	// fenced; resume seeds the exact recorded policy before promoting the helper.
	return s.writeAuthority("host-adoption.json", envelope)
}

func (s *Store) seedAdoptionPolicy(ctx context.Context, operation Adoption) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	envelope, actual, err := s.adoption()
	if err != nil {
		return err
	}
	if envelope.Confirmed || !reflect.DeepEqual(actual, operation) {
		return errors.New("update: adoption changed before policy publication")
	}
	policy, err := s.hostPolicy()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(policy, operation.Policy) {
		return nil
	}
	if policy.Revision+1 != operation.Policy.Revision {
		return errors.New("update: adoption policy was changed locally")
	}
	return s.writeAuthority("host-policy.json", operation.Policy)
}

func (s *Store) saveAdoption(ctx context.Context, previous, operation Adoption) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	envelope, actual, err := s.adoption()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, previous) || operation.ID != previous.ID || operation.Pin != previous.Pin {
		return errors.New("update: adoption changed during work")
	}
	if err := encodeAdoption(&envelope, operation); err != nil {
		return err
	}
	if operation.Phase == AdoptionRolledBack {
		if err := s.failedAdoptionEdge(operation); err != nil {
			return err
		}
	}
	return s.writeAuthority("host-adoption.json", envelope)
}

func canReplaceAdoption(prior Adoption, version string) error {
	if prior.Phase != AdoptionRolledBack && prior.Phase != AdoptionAttention {
		return errors.New("update: another adoption already owns this installation")
	}
	order, err := release.CompareVersions(version, prior.Version)
	if err != nil || order <= 0 {
		return errors.New("update: restarting failed adoption requires a newer pinned release")
	}
	return nil
}

func (s *Store) failedAdoptionEdge(operation Adoption) error {
	artifact, err := operation.Request.Manifest.Artifact(operation.Request.Current.Platform)
	if err != nil {
		return hostError(err)
	}
	failed := FailedEdge{Edge: Edge{operation.Request.Current.Digest, artifact.BinarySHA256, operation.Request.Current.Platform}, Version: operation.Version, Phase: updateinstall.RolledBack, Problem: operation.Problem, At: time.Now().UTC()}
	return s.writeAuthority(failedEdgeName(failed.Edge), failed)
}

func AdoptionMilestones(envelope HostAdoption, operation, pin string) error {
	if err := envelope.validate(); err != nil {
		return err
	}
	if envelope.Operation != operation || envelope.Pin != pin || !envelope.Confirmed || envelope.Adopted == nil || envelope.Unattended == nil {
		return errors.New("update: pinned adoption and later unattended commit are required")
	}
	if envelope.Adopted.Workers != envelope.Adopted.WorkersPreserved || envelope.Unattended.Workers != envelope.Unattended.WorkersPreserved {
		return errors.New("update: adoption predecessor did not preserve all workers")
	}
	return nil
}

// AdoptionOrder makes the initial last host depend on every other member. The
// ordinary policy may have fewer predecessors, so a sleeping laptop cannot hold
// later updates on the starting host.
func AdoptionOrder(fleet Fleet, canaries []string, last string) (map[string][]Host, error) {
	if err := fleet.Validate(); err != nil {
		return nil, err
	}
	known := make(map[string]Host, len(fleet.Members))
	for _, host := range fleet.Members {
		known[host.ID] = host
	}
	if known[last].ID == "" || len(canaries) == 0 {
		return nil, errors.New("update: adoption requires canaries and a last host in the fleet")
	}
	for i, id := range canaries {
		if known[id].ID == "" || id == last || slices.Contains(canaries[:i], id) {
			return nil, errors.New("update: invalid adoption canary")
		}
	}
	result := make(map[string][]Host, len(known))
	for _, host := range fleet.Members {
		result[host.ID] = adoptionPredecessors(fleet.Members, canaries, last, host.ID)
	}
	return result, nil
}

func adoptionPredecessors(hosts []Host, canaries []string, last, id string) []Host {
	if slices.Contains(canaries, id) {
		return nil
	}
	var required []Host
	for _, host := range hosts {
		if host.ID != id && (id == last || slices.Contains(canaries, host.ID)) {
			required = append(required, host)
		}
	}
	return required
}
