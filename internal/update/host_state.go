package update

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

type HostPhase string

const (
	HostIdle        HostPhase = "idle"
	HostDiscovering HostPhase = "discovering"
	HostWaiting     HostPhase = "waiting"
	HostInstalling  HostPhase = "installing"
	HostAttention   HostPhase = "attention"
)

type Edge struct {
	From     string           `json:"from"`
	To       string           `json:"to"`
	Platform release.Platform `json:"platform"`
}

type OwnedRequest struct {
	Request        string                 `json:"request"`
	Generation     uint64                 `json:"generation"`
	PolicyRevision uint64                 `json:"policyRevision"`
	IntentRevision uint64                 `json:"intentRevision,omitempty"`
	Edge           Edge                   `json:"edge"`
	Installation   updateinstall.Request  `json:"installation"`
	Metadata       string                 `json:"metadata"`
	Position       release.Position       `json:"position"`
	Epoch          uint32                 `json:"epoch"`
	Settings       updateinstall.Settings `json:"settings"`
}

type HostProblem struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type HostUpdate struct {
	Schema         int                 `json:"schema"`
	Phase          HostPhase           `json:"phase"`
	Owned          *OwnedRequest       `json:"owned,omitempty"`
	Problem        *HostProblem        `json:"problem,omitempty"`
	ProblemSince   time.Time           `json:"problemSince,omitempty"`
	NextAttempt    time.Time           `json:"nextAttempt,omitempty"`
	Failures       uint32              `json:"failures,omitempty"`
	LastRequest    string              `json:"lastRequest,omitempty"`
	LastGeneration uint64              `json:"lastGeneration,omitempty"`
	LastPhase      updateinstall.Phase `json:"lastPhase,omitempty"`
}

type FailedEdge struct {
	Edge    Edge                `json:"edge"`
	Version string              `json:"version"`
	Phase   updateinstall.Phase `json:"phase"`
	Problem string              `json:"problem"`
	At      time.Time           `json:"at"`
}

type LedgerEntry struct {
	Seq              uint64    `json:"seq"`
	Request          string    `json:"request"`
	Generation       uint64    `json:"generation"`
	Version          string    `json:"version"`
	Metadata         string    `json:"metadata,omitempty"`
	Binary           string    `json:"binary"`
	From             string    `json:"from"`
	Origin           string    `json:"origin"`
	CommittedAt      time.Time `json:"committedAt"`
	Workers          int       `json:"workers"`
	WorkersPreserved int       `json:"workersPreserved"`
}

type hostLedger struct {
	Schema int    `json:"schema"`
	First  uint64 `json:"first"`
	Last   uint64 `json:"last"`
}

type hostLedgerIdentity struct {
	Seq uint64 `json:"seq"`
}

func ledgerIdentityName(entry LedgerEntry) string {
	return "host-ledger-request-" + discoveryDigest([]byte(fmt.Sprintf("%s:%d", entry.Request, entry.Generation))) + ".json"
}

// HostAdoption milestones live separately from the pageable ledger until the fleet
// operation settles. Confirmation must be durable before unattended work starts.
type HostAdoption struct {
	Schema     int             `json:"schema"`
	Operation  string          `json:"operation"`
	Pin        string          `json:"pin"`
	Confirmed  bool            `json:"confirmed"`
	Settled    bool            `json:"settled"`
	Adopted    *LedgerEntry    `json:"adopted,omitempty"`
	Unattended *LedgerEntry    `json:"unattended,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

func (a HostAdoption) validate() error {
	if a.Schema != 1 || !ValidRunID(a.Operation) || !hostDigest(a.Pin) {
		return errors.New("update: invalid adoption identity")
	}
	if len(a.Details) > 64<<10 || len(a.Details) > 0 && !json.Valid(a.Details) {
		return errors.New("update: adoption details exceed their bound or contain invalid JSON")
	}
	if a.Confirmed && (a.Adopted == nil || a.Adopted.Metadata != a.Pin || a.Adopted.Origin != "adoption") {
		return errors.New("update: adoption confirmation lacks pinned commit")
	}
	if a.Adopted != nil {
		if err := validateAdoptionEntry(*a.Adopted, "adoption"); err != nil {
			return err
		}
		if a.Adopted.Metadata != a.Pin {
			return errors.New("update: adoption milestone differs from activation metadata pin")
		}
	}
	if a.Unattended != nil {
		return a.validateUnattended()
	}
	return nil
}

func validateAdoptionEntry(entry LedgerEntry, origin string) error {
	if entry.Request == "" || len(entry.Request) > 96 || entry.Generation == 0 || entry.Origin != origin || !hostDigest(entry.Metadata) || !hostDigest(entry.Binary) || entry.CommittedAt.IsZero() || entry.Workers < 0 || entry.WorkersPreserved < 0 || entry.WorkersPreserved > entry.Workers {
		return errors.New("update: adoption milestone lacks committed ledger identity")
	}
	if _, err := release.CompareVersions(entry.Version, entry.Version); err != nil {
		return hostError(err)
	}
	return nil
}

func (a HostAdoption) validateUnattended() error {
	if !a.Confirmed || a.Adopted == nil {
		return errors.New("update: unattended milestone precedes adoption confirmation")
	}
	if err := validateAdoptionEntry(*a.Unattended, hostOrigin); err != nil {
		return err
	}
	order, err := release.CompareVersions(a.Unattended.Version, a.Adopted.Version)
	if err != nil || order <= 0 || a.Unattended.Binary == a.Adopted.Binary || !a.Unattended.CommittedAt.After(a.Adopted.CommittedAt) {
		return errors.New("update: unattended milestone does not follow the pinned adoption")
	}
	return nil
}

func (s *Store) hostUpdate() (HostUpdate, error) {
	var state HostUpdate
	err := s.readAuthority("host-update.json", &state)
	if errors.Is(err, os.ErrNotExist) {
		return HostUpdate{Schema: 1, Phase: HostIdle}, nil
	}
	if err != nil {
		return state, err
	}
	if state.Schema != 1 || !validHostPhase(state.Phase) {
		return state, errors.New("update: invalid host update state")
	}
	if state.Phase == HostInstalling && state.Owned == nil {
		return state, errors.New("update: installing host work has no durable ownership")
	}
	if state.Owned != nil {
		if state.Phase != HostInstalling {
			return state, errors.New("update: ownership outside installing phase")
		}
		if err := state.Owned.matches(state.Owned.Installation); err != nil {
			return state, err
		}
	}
	return state, nil
}

func validHostPhase(phase HostPhase) bool {
	return phase == HostIdle || phase == HostDiscovering || phase == HostWaiting || phase == HostInstalling || phase == HostAttention
}

func (o OwnedRequest) matches(request updateinstall.Request) error {
	artifact, err := request.Manifest.Artifact(request.Current.Platform)
	if err != nil {
		return hostError(err)
	}
	if !ValidRunID(o.Request) || o.Generation == 0 || o.PolicyRevision == 0 && o.IntentRevision == 0 || o.Epoch == 0 || !hostDigest(o.Metadata) || request.Automatic != nil {
		return errors.New("update: invalid host ownership")
	}
	if o.Request != request.ID || o.Generation != request.Generation || o.Edge != (Edge{request.Current.Digest, artifact.BinarySHA256, request.Current.Platform}) || !reflect.DeepEqual(o.Installation, request) {
		return errors.New("update: request differs from durable host ownership")
	}
	if !hostDigest(o.Edge.From) || !hostDigest(o.Edge.To) {
		return errors.New("update: invalid owned edge")
	}
	if o.Settings.StateDir == "" || o.Settings.Executable == "" || o.Settings.CacheDir == "" || o.Settings.ClientOnly {
		return errors.New("update: host ownership lacks daemon installation settings")
	}
	return nil
}

func hostDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == digest
}

func (s *Store) HostState(ctx context.Context) (HostUpdate, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostUpdate{}, err
	}
	defer unlock()
	return s.hostUpdate()
}

func (s *Store) ledgerHead() (hostLedger, error) {
	var head hostLedger
	err := s.readAuthority("host-ledger.json", &head)
	if errors.Is(err, os.ErrNotExist) {
		return hostLedger{Schema: 1, First: 1}, nil
	}
	if err != nil {
		return head, err
	}
	if head.Schema != 1 || head.First == 0 || head.Last == math.MaxUint64 || head.First > head.Last+1 {
		return head, errors.New("update: invalid ledger head")
	}
	return head, nil
}

func ledgerName(seq uint64) string { return fmt.Sprintf("host-ledger-%020d.json", seq) }

// appendLedger runs under authority. Publishing the entry before the head lets
// a crash repeat the same sequence and request without losing or duplicating it.
func (s *Store) appendLedger(entry LedgerEntry) error {
	head, err := s.ledgerHead()
	if err != nil {
		return err
	}
	if found, err := s.existingLedgerIdentity(head, entry); err != nil || found {
		return err
	}
	if found, err := s.recoverLedgerTail(head, entry); found || err != nil {
		return err
	}
	entry.Seq = head.Last + 1
	if entry.Seq == math.MaxUint64 {
		return errors.New("update: ledger sequence exhausted")
	}
	var pending LedgerEntry
	err = s.readAuthority(ledgerName(entry.Seq), &pending)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (pending.Request != entry.Request || pending.Generation != entry.Generation) {
		return errors.New("update: unsettled ledger entry belongs to another request")
	}
	if err := s.writeAuthority(ledgerName(entry.Seq), entry); err != nil {
		return err
	}
	head.Last = entry.Seq
	if err := s.writeAuthority("host-ledger.json", head); err != nil {
		return err
	}
	return s.writeAuthority(ledgerIdentityName(entry), hostLedgerIdentity{Seq: entry.Seq})
}

func (s *Store) recoverLedgerTail(head hostLedger, entry LedgerEntry) (bool, error) {
	if head.Last == 0 {
		return false, nil
	}
	var last LedgerEntry
	if err := s.readAuthority(ledgerName(head.Last), &last); err != nil {
		return false, err
	}
	if last.Request != entry.Request || last.Generation != entry.Generation {
		return false, nil
	}
	return true, s.writeAuthority(ledgerIdentityName(entry), hostLedgerIdentity{Seq: last.Seq})
}

func (s *Store) existingLedgerIdentity(head hostLedger, entry LedgerEntry) (bool, error) {
	var identity hostLedgerIdentity
	err := s.readAuthority(ledgerIdentityName(entry), &identity)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if identity.Seq < head.First {
		return true, nil
	}
	if identity.Seq > head.Last {
		return false, errors.New("update: ledger request identity outside retained history")
	}
	var existing LedgerEntry
	if err := s.readAuthority(ledgerName(identity.Seq), &existing); err != nil {
		return false, err
	}
	if existing.Seq != identity.Seq || existing.Request != entry.Request || existing.Generation != entry.Generation {
		return false, errors.New("update: ledger request identity differs from entry")
	}
	return true, nil
}

func (s *Store) HostLedger(ctx context.Context, after uint64, limit int) ([]LedgerEntry, bool, error) {
	if limit < 1 || limit > 64 {
		return nil, false, errors.New("update: ledger page limit must be 1..64")
	}
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	return s.hostLedgerPage(after, limit)
}

func (s *Store) hostLedgerPage(after uint64, limit int) ([]LedgerEntry, bool, error) {
	head, err := s.ledgerHead()
	if err != nil {
		return nil, false, err
	}
	if after >= head.Last {
		return nil, false, nil
	}
	entries := make([]LedgerEntry, 0, limit)
	seq := max(head.First, after+1)
	for ; seq <= head.Last && len(entries) < limit; seq++ {
		var entry LedgerEntry
		if err := s.readAuthority(ledgerName(seq), &entry); err != nil {
			return nil, false, err
		}
		if entry.Seq != seq {
			return nil, false, errors.New("update: ledger sequence differs")
		}
		entries = append(entries, entry)
	}
	return entries, seq <= head.Last, nil
}

func failedEdgeName(edge Edge) string {
	return "host-failed-" + edge.From + "-" + edge.To + "-" + edge.Platform.OS + "-" + edge.Platform.Arch + ".json"
}

func (s *Store) failedHostEdge(edge Edge) (bool, error) {
	var failed FailedEdge
	err := s.readAuthority(failedEdgeName(edge), &failed)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if failed.Edge != edge || failed.Phase != updateinstall.RolledBack {
		return false, errors.New("update: invalid failed edge record")
	}
	return true, nil
}

func (s *Store) pruneFailedSource(source string) error {
	if !hostDigest(source) {
		return errors.New("update: invalid failed-edge source")
	}
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for {
		entries, readErr := dir.ReadDir(64)
		if err := pruneFailedEntries(dir, entries, source); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return syncHostAuthority(dir)
		}
		if readErr != nil {
			return fmt.Errorf("update: read failed-edge records: %w", readErr)
		}
	}
}

func pruneFailedEntries(dir *os.File, entries []os.DirEntry, source string) error {
	for _, entry := range entries {
		if err := removeFailedSource(dir, entry.Name(), source); err != nil {
			return err
		}
	}
	return nil
}

func removeFailedSource(dir *os.File, name, source string) error {
	if !strings.HasPrefix(name, "host-failed-"+source+"-") || !strings.HasSuffix(name, ".json") {
		return nil
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("update: prune settled failed edge: %w", err)
	}
	return nil
}

func syncHostAuthority(dir *os.File) error {
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("update: sync failed-edge retention: %w", err)
	}
	return nil
}
