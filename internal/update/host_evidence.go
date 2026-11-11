package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

const hostOrigin = "host"

type EvidenceRequest struct {
	Settled     string `json:"settled,omitempty"`
	AfterSeq    uint64 `json:"afterSeq"`
	Limit       int    `json:"limit"`
	Drop        bool   `json:"drop,omitempty"`
	FailedAfter string `json:"failedAfter,omitempty"`
}

type EvidenceReply struct {
	Schema     int           `json:"schema"`
	ID         string        `json:"id"`
	Mode       HostMode      `json:"mode"`
	Phase      HostPhase     `json:"phase"`
	Entries    []LedgerEntry `json:"entries"`
	More       bool          `json:"more"`
	Failed     []FailedEdge  `json:"failed,omitempty"`
	FailedNext string        `json:"failedNext,omitempty"`
	Attention  string        `json:"attention,omitempty"`
	Adoption   *HostAdoption `json:"adoption,omitempty"`
}

type HostDependent struct {
	ID        string    `json:"id"`
	Settled   string    `json:"settled"`
	UpdatedAt time.Time `json:"updatedAt"`
	Retained  uint64    `json:"retained"`
}

type hostDependents struct {
	Schema  int             `json:"schema"`
	Entries []HostDependent `json:"entries"`
}

func validEvidenceRequest(r EvidenceRequest) error {
	if r.Limit < 1 || r.Limit > 64 || len(r.FailedAfter) > 256 || strings.ContainsAny(r.FailedAfter, "/\\") {
		return errors.New("update: invalid evidence page bounds")
	}
	if r.Drop && r.Settled != "" {
		return errors.New("update: drop cannot acknowledge a version")
	}
	if r.Settled != "" {
		return validEvidenceVersion(r.Settled)
	}
	return nil
}

func validEvidenceVersion(version string) error {
	if len(version) > release.MaxVersionBytes {
		return errors.New("update: evidence version exceeds bound")
	}
	_, err := release.CompareVersions(version, version)
	return hostError(err)
}

// HostEvidence runs only after device authentication. Acknowledgements change
// retention, never update policy or installation authority.
func (s *Store) HostEvidence(ctx context.Context, dependent string, request EvidenceRequest) (EvidenceReply, error) {
	if _, err := PublicKey(dependent); err != nil {
		return EvidenceReply{}, err
	}
	if err := validEvidenceRequest(request); err != nil {
		return EvidenceReply{}, err
	}
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return EvidenceReply{}, err
	}
	defer unlock()
	if err := s.acknowledgeHost(dependent, request); err != nil {
		return EvidenceReply{}, err
	}
	policy, err := s.hostPolicy()
	if err != nil {
		return EvidenceReply{}, err
	}
	state, err := s.hostUpdate()
	if err != nil {
		return EvidenceReply{}, err
	}
	reply := EvidenceReply{Schema: 1, Mode: policy.Mode, Phase: state.Phase}
	if state.Phase == HostAttention && state.Problem != nil {
		reply.Attention = boundedProblem(state.Problem.Detail)
	}
	reply.Entries, reply.More, err = s.hostLedgerPage(request.AfterSeq, request.Limit)
	if err != nil {
		return reply, err
	}
	reply.Failed, reply.FailedNext, err = s.hostFailedPage(request.FailedAfter, request.Limit)
	if err != nil {
		return reply, err
	}
	var adoption HostAdoption
	err = s.readAuthority("host-adoption.json", &adoption)
	if errors.Is(err, os.ErrNotExist) {
		return reply, nil
	}
	if err != nil {
		return reply, err
	}
	if err := adoption.validate(); err != nil {
		return reply, err
	}
	if !adoption.Settled {
		reply.Adoption = &adoption
	}
	return reply, nil
}

func (s *Store) readHostDependents() (hostDependents, error) {
	value := hostDependents{Schema: 1}
	err := s.readAuthority("host-dependents.json", &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	if value.Schema != 1 || len(value.Entries) > MaximumHosts {
		return value, errors.New("update: invalid dependent record")
	}
	seen := make(map[string]bool, len(value.Entries))
	for _, entry := range value.Entries {
		if _, err := PublicKey(entry.ID); err != nil {
			return value, err
		}
		if seen[entry.ID] || entry.UpdatedAt.IsZero() {
			return value, errors.New("update: invalid dependent identity")
		}
		if err := validEvidenceVersion(entry.Settled); err != nil {
			return value, err
		}
		seen[entry.ID] = true
	}
	return value, nil
}

func (s *Store) acknowledgeHost(id string, request EvidenceRequest) error {
	if request.Settled == "" && !request.Drop {
		return nil
	}
	deps, err := s.readHostDependents()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(deps.Entries, func(d HostDependent) bool { return d.ID == id })
	if request.Drop {
		if index >= 0 {
			deps.Entries = slices.Delete(deps.Entries, index, index+1)
		}
	} else {
		deps.Entries, err = acknowledgeDependent(deps.Entries, index, id, request.Settled)
		if err != nil {
			return err
		}
	}
	if err := s.writeAuthority("host-dependents.json", deps); err != nil {
		return err
	}
	return s.pruneHostLedger(deps.Entries)
}

func acknowledgeDependent(entries []HostDependent, index int, id, settled string) ([]HostDependent, error) {
	if index >= 0 {
		order, err := release.CompareVersions(settled, entries[index].Settled)
		if err != nil || order < 0 {
			return nil, errors.New("update: dependent settlement moved backwards")
		}
		entries[index].Settled, entries[index].UpdatedAt = settled, time.Now().UTC()
		return entries, nil
	}
	if len(entries) >= MaximumHosts {
		return nil, errors.New("update: dependent capacity reached")
	}
	return append(entries, HostDependent{ID: id, Settled: settled, UpdatedAt: time.Now().UTC()}), nil
}

func (s *Store) HostDependents(ctx context.Context) ([]HostDependent, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	deps, err := s.readHostDependents()
	if err != nil {
		return nil, err
	}
	head, err := s.ledgerHead()
	if err != nil {
		return nil, err
	}
	for i := range deps.Entries {
		count, err := s.retainedForDependent(head, deps.Entries[i].Settled)
		if err != nil {
			return nil, err
		}
		deps.Entries[i].Retained = count
	}
	return deps.Entries, nil
}

func (s *Store) retainedForDependent(head hostLedger, settled string) (uint64, error) {
	var count uint64
	for seq := head.First; seq < head.Last; seq++ {
		var entry LedgerEntry
		if err := s.readAuthority(ledgerName(seq), &entry); err != nil {
			return 0, err
		}
		order, err := release.CompareVersions(entry.Version, settled)
		if err != nil {
			return 0, hostError(err)
		}
		if order > 0 {
			count++
		}
	}
	return count, nil
}

func (s *Store) ForgetHostDependent(ctx context.Context, id string) error {
	if _, err := PublicKey(id); err != nil {
		return err
	}
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.acknowledgeHost(id, EvidenceRequest{Drop: true})
}

func (s *Store) pruneHostLedger(deps []HostDependent) error {
	head, err := s.ledgerHead()
	if err != nil || head.Last == 0 {
		return err
	}
	first := head.First
	for first < head.Last {
		var entry LedgerEntry
		if err := s.readAuthority(ledgerName(first), &entry); err != nil {
			return err
		}
		held, err := ledgerHeld(entry, deps)
		if err != nil {
			return err
		}
		if held {
			break
		}
		first++
	}
	prior := head.First
	head.First = first
	if err := s.writeAuthority("host-ledger.json", head); err != nil {
		return err
	}
	return s.removeLedgerPrefix(prior, first)
}

func ledgerHeld(entry LedgerEntry, deps []HostDependent) (bool, error) {
	for _, dependent := range deps {
		order, err := release.CompareVersions(entry.Version, dependent.Settled)
		if err != nil {
			return true, hostError(err)
		}
		if order > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) removeLedgerPrefix(_ uint64, first uint64) error {
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return hostError(err)
	}
	cutoff := ledgerName(first)
	for _, name := range names {
		if !strings.HasPrefix(name, "host-ledger-") || len(name) != len(cutoff) || name >= cutoff {
			continue
		}
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("update: prune ledger: %w", err)
		}
	}
	return syncHostAuthority(dir)
}

func (s *Store) hostFailedPage(after string, limit int) ([]FailedEdge, string, error) {
	dir, err := s.authorityDirectory()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, "", fmt.Errorf("update: read failure names: %w", err)
	}
	names = slices.DeleteFunc(names, func(name string) bool {
		return name <= after || !strings.HasPrefix(name, "host-failed-") || !strings.HasSuffix(name, ".json")
	})
	slices.Sort(names)
	result := make([]FailedEdge, 0, min(limit, len(names)))
	for _, name := range names[:min(limit, len(names))] {
		var failed FailedEdge
		if err := s.readAuthority(name, &failed); err != nil {
			return nil, "", err
		}
		if err := validEvidenceFailure(failed); err != nil {
			return nil, "", err
		}
		if failedEdgeName(failed.Edge) != name {
			return nil, "", errors.New("update: failure filename differs from edge")
		}
		result = append(result, failed)
	}
	if len(names) > limit {
		return result, names[limit-1], nil
	}
	return result, "", nil
}

func validEvidenceEntry(entry LedgerEntry) error {
	if entry.Seq == 0 || !ValidRunID(entry.Request) || entry.Generation == 0 || entry.Metadata != "" && !hostDigest(entry.Metadata) || entry.Metadata == "" && entry.Origin != string(HostManual) || !hostDigest(entry.Binary) || !hostDigest(entry.From) || entry.CommittedAt.IsZero() || entry.Workers < 0 || entry.Workers > MaxHostReaderWorkers || entry.WorkersPreserved < 0 || entry.WorkersPreserved > entry.Workers || entry.Origin != hostOrigin && entry.Origin != "adoption" && entry.Origin != string(HostManual) {
		return errors.New("update: invalid committed evidence entry")
	}
	return validEvidenceVersion(entry.Version)
}

func validEvidenceFailure(failed FailedEdge) error {
	if !hostDigest(failed.Edge.From) || !hostDigest(failed.Edge.To) || failed.Edge.Platform.OS == "" || failed.Edge.Platform.Arch == "" || len(failed.Edge.Platform.OS) > 16 || len(failed.Edge.Platform.Arch) > 16 || stringsContainsPath(failed.Edge.Platform.OS) || stringsContainsPath(failed.Edge.Platform.Arch) || failed.Phase != updateinstall.RolledBack || failed.At.IsZero() || len(failed.Problem) > 512 {
		return errors.New("update: invalid failed-edge evidence")
	}
	return validEvidenceVersion(failed.Version)
}
