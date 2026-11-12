package update

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type HostObservedRelease struct {
	Epoch       uint32    `json:"epoch"`
	Version     string    `json:"version"`
	Metadata    string    `json:"metadata"`
	PublishedAt time.Time `json:"publishedAt"`
}

type HostDiscoveryObservation struct {
	Schema       int                 `json:"schema"`
	Source       release.Build       `json:"source"`
	Epoch        uint32              `json:"epoch"`
	LatestEpoch  uint32              `json:"latestEpoch"`
	Head         HostObservedRelease `json:"head"`
	Target       HostObservedRelease `json:"target"`
	ObservedAt   time.Time           `json:"observedAt"`
	PendingSince time.Time           `json:"pendingSince"`
}

type HostStatus struct {
	Policy      HostPolicy
	State       HostUpdate
	Discovery   *HostDiscoveryObservation
	Last        *LedgerEntry
	Peers       []HostPeer
	LocalTarget *HostTargetStatus
}

func hostProblemSince(prior, next HostUpdate, now time.Time) time.Time {
	if next.Problem == nil {
		return time.Time{}
	}
	if prior.Problem != nil && *prior.Problem == *next.Problem && !prior.ProblemSince.IsZero() {
		return prior.ProblemSince
	}
	return now
}

// HostStatus reads advisory facts under one authority snapshot. It never grants
// installation permission or acknowledges another host's retained history.
func (s *Store) HostStatus(ctx context.Context) (HostStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostStatus{}, err
	}
	defer unlock()
	status := HostStatus{}
	status.Policy, err = s.hostPolicy()
	if err != nil {
		return status, err
	}
	status.State, err = s.hostUpdate()
	if err != nil {
		return status, err
	}
	status.Discovery, err = s.hostDiscoveryObservation()
	if err != nil {
		return status, err
	}
	status.Last, err = s.lastPinnedHostCommit()
	if err != nil {
		return status, err
	}
	peers, err := s.readHostPeers()
	status.Peers = peers.Entries
	if err != nil {
		return status, err
	}
	intent, err := s.hostTarget()
	if intent != nil {
		status.LocalTarget = &intent.Status
	}
	return status, err
}

func (s *Store) hostDiscoveryObservation() (*HostDiscoveryObservation, error) {
	var observation HostDiscoveryObservation
	err := s.readAuthority("host-status.json", &observation)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if observation.Schema != 1 || !hostDigest(observation.Source.Digest) || observation.Epoch == 0 || observation.LatestEpoch < observation.Epoch || !hostDigest(observation.Head.Metadata) || !hostDigest(observation.Target.Metadata) || observation.ObservedAt.IsZero() {
		return nil, errors.New("update: invalid advisory discovery observation")
	}
	if err := validEvidenceVersion(observation.Head.Version); err != nil {
		return nil, err
	}
	if err := validEvidenceVersion(observation.Target.Version); err != nil {
		return nil, err
	}
	return &observation, nil
}

func (s *Store) lastPinnedHostCommit() (*LedgerEntry, error) {
	head, err := s.ledgerHead()
	if err != nil {
		return nil, err
	}
	for seq, read := head.Last, 0; seq >= head.First && read < 64; seq, read = seq-1, read+1 {
		var entry LedgerEntry
		if err := s.readAuthority(ledgerName(seq), &entry); err != nil {
			return nil, err
		}
		if err := validEvidenceEntry(entry); err != nil {
			return nil, err
		}
		if entry.Metadata != "" {
			return &entry, nil
		}
	}
	return nil, nil
}

func observedHostRelease(data []byte) (HostObservedRelease, error) {
	metadata, err := release.DecodeUpdateMetadata(data)
	if err != nil {
		return HostObservedRelease{}, hostError(err)
	}
	return HostObservedRelease{Epoch: metadata.Epoch, Version: metadata.Version, Metadata: discoveryDigest(data), PublishedAt: metadata.PublishedAt}, nil
}

// selected is supplied only after the discovery pass authenticates both the
// epoch head and its policy-limited target. Partial cursors supply no status.
func (u *HostUpdater) observeDiscovery(ctx context.Context, current release.Build, source HostSource, step DiscoveryStep, selected []byte) error {
	if len(selected) == 0 {
		return nil
	}
	if err := verifyHostSource(current, source); err != nil {
		return err
	}
	if discoveryDigest(step.Cursor.Metadata) != step.Cursor.Head.SHA256 {
		return errors.New("update: advisory head differs from authenticated discovery pin")
	}
	observation := HostDiscoveryObservation{Schema: 1, Source: current, Epoch: source.Epoch, LatestEpoch: step.Cursor.LatestEpoch, ObservedAt: u.now()}
	var err error
	observation.Head, err = observedHostRelease(step.Cursor.Metadata)
	if err != nil {
		return err
	}
	observation.Target, err = observedHostRelease(selected)
	if err != nil {
		return err
	}
	if observation.Head.Version != step.Cursor.Head.Tag || observation.Head.Epoch != source.Epoch || observation.Target.Epoch != source.Epoch || observation.LatestEpoch < source.Epoch {
		return errors.New("update: advisory target or head differs from the verified source epoch")
	}
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	previous, err := u.Store.hostDiscoveryObservation()
	if err != nil {
		return err
	}
	if newerHostVersion(observation.Target.Version, current.Version) {
		observation.PendingSince = observation.ObservedAt
	}
	if previous != nil && !previous.PendingSince.IsZero() && previous.Target.Metadata == observation.Target.Metadata && newerHostVersion(observation.Target.Version, current.Version) {
		observation.PendingSince = previous.PendingSince
	}
	return u.Store.writeAuthority("host-status.json", observation)
}
