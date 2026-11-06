package hostprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type Publication string

const (
	PublicationUnchanged Publication = "unchanged"
	BeforeReplicaRename  Publication = "before-replica-rename"
	VisibleUnconfirmed   Publication = "visible-unconfirmed"
	ReplicaDurable       Publication = "replica-durable"
	PublicationComplete  Publication = "complete"
)

type FleetWrite struct {
	Publication Publication
	Revision    uint64
	Digest      string
	Changed     bool
	Outbox      Outbox
}

func (r FleetWrite) Committed() bool { return r.Publication == ReplicaDurable || r.Complete() }
func (r FleetWrite) Complete() bool {
	return r.Publication == PublicationComplete || r.Publication == PublicationUnchanged
}

type FleetStore struct {
	dir      string
	now      func() time.Time
	syncDir  func(*os.File) error
	syncFile func(*os.File) error
	write    func(*os.File, []byte) error
	rename   func(*os.File, string, string) error
}

func OpenFleetStore(stateDir string) *FleetStore {
	return &FleetStore{dir: stateDir, now: time.Now, syncDir: syncFile, syncFile: syncFile, write: writeAll, rename: renameFleetFile}
}
func renameFleetFile(dir *os.File, from, to string) error {
	if err := unix.Renameat(int(dir.Fd()), from, int(dir.Fd()), to); err != nil {
		return fmt.Errorf("publish fleet %s: %w", to, err)
	}
	return nil
}

// Read is passive. Visible bytes are observations, not durability acknowledgements.
func (s *FleetStore) Read(trusted func(string) bool) (Snapshot[FleetDocument], Outbox, error) {
	snapshot, err := s.ReadReplica(trusted)
	if err != nil || snapshot.State == StateAbsent {
		return snapshot, Outbox{Members: []Delivery{}}, err
	}
	parent, err := openDirectory(s.dir, false)
	if err != nil {
		return snapshot, Outbox{}, err
	}
	defer func() { _ = parent.Close() }()
	dir, err := openPrivateDirectoryChild(parent, "fleet", false)
	if err != nil {
		return snapshot, Outbox{}, err
	}
	defer func() { _ = dir.Close() }()
	out, err := readOutbox(dir)
	if err != nil {
		return snapshot, Outbox{Members: []Delivery{}}, fmt.Errorf("read fleet delivery state: %w", err)
	}
	if snapshot.Value == nil || out.Digest != snapshot.Value.Digest() || out.Revision != snapshot.Value.Revision {
		return snapshot, Outbox{Members: []Delivery{}}, errors.New("fleet outbox does not describe the visible replica; run mesh profile status --fleet --retry --json to reconcile")
	}
	return snapshot, out, nil
}

func (s *FleetStore) locked(ctx context.Context, run func(*os.File) (FleetWrite, error)) (FleetWrite, error) {
	result := FleetWrite{Publication: BeforeReplicaRename}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("fleet operation cancelled: %w", err)
	}
	// State is initialized by identity setup, never by a fleet read or delivery loop.
	parent, err := openDirectory(s.dir, false)
	if err != nil {
		return result, err
	}
	defer func() { _ = parent.Close() }()
	dir, err := openPrivateDirectoryChild(parent, "fleet", true)
	if err != nil {
		return result, err
	}
	defer func() { _ = dir.Close() }()
	lock, err := acquireFleetLock(ctx, dir)
	if err != nil {
		return result, err
	}
	defer func() { _ = lock.Close() }()
	if err := s.syncDir(parent); err != nil {
		return result, fmt.Errorf("sync fleet directory parent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("fleet operation cancelled: %w", err)
	}
	return run(dir)
}

func (s *FleetStore) recover(dir *os.File, scope FleetScope) (Snapshot[FleetDocument], FleetWrite, error) {
	snapshot, err := readReplica(dir, scope.Trusted)
	result := FleetWrite{Publication: BeforeReplicaRename, Outbox: Outbox{Members: []Delivery{}}}
	if snapshot.Revision != nil {
		result.Revision = *snapshot.Revision
	}
	if err != nil {
		return snapshot, result, err
	}
	if snapshot.State == StateAbsent {
		return snapshot, result, nil
	}
	if snapshot.Value == nil {
		return snapshot, result, nil
	}
	result.Digest = snapshot.Value.Digest()
	result.Publication = VisibleUnconfirmed
	if err := s.settleFile(dir, "profile.json"); err != nil {
		return snapshot, result, err
	}
	result.Publication = ReplicaDurable
	out, err := s.repairOutbox(dir, *snapshot.Value, scope.Members)
	if err != nil {
		return snapshot, result, err
	}
	result.Outbox = out
	result.Publication = PublicationUnchanged
	return snapshot, result, nil
}

func (s *FleetStore) repairOutbox(dir *os.File, doc FleetDocument, members []string) (Outbox, error) {
	ids, err := fleetMembers(members)
	if err != nil {
		return Outbox{}, err
	}
	previous, err := readOutbox(dir)
	var validation *ValidationError
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.As(err, &validation) {
		return Outbox{}, err
	}
	out := reconcileOutbox(previous, doc, ids, s.now())
	if !sameOutbox(previous, out) {
		return out, s.saveOutbox(dir, out)
	}
	return out, s.settleFile(dir, "outbox.json")
}

func (s *FleetStore) settleFile(dir *os.File, name string) error {
	file, err := openPrivateFile(dir, name, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("sync visible fleet %s: %w", name, err)
	}
	if err := s.syncDir(dir); err != nil {
		return fmt.Errorf("fleet %s visible but not confirmed durable: %w; repeat the operation", name, err)
	}
	return nil
}

func (s *FleetStore) publishFleetFile(dir *os.File, target string, data []byte) (bool, error) {
	file, name, err := temporaryFile(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	defer func() { _ = file.Close() }()
	if err := s.write(file, data); err != nil {
		return false, fmt.Errorf("write fleet %s: %w", target, err)
	}
	if err := s.syncFile(file); err != nil {
		return false, fmt.Errorf("sync fleet %s: %w", target, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close fleet %s: %w", target, err)
	}
	if err := s.rename(dir, name, target); err != nil {
		return false, err
	}
	if err := s.syncDir(dir); err != nil {
		return true, fmt.Errorf("fleet %s visible but not confirmed durable: %w; repeat the operation", target, err)
	}
	return true, nil
}
func (s *FleetStore) publishDocument(dir *os.File, doc FleetDocument, scope FleetScope) (FleetWrite, error) {
	result := FleetWrite{Publication: BeforeReplicaRename, Revision: doc.Revision, Digest: doc.Digest()}
	data, err := json.Marshal(doc)
	if err != nil {
		return result, fmt.Errorf("encode fleet document: %w", err)
	}
	visible, err := s.publishFleetFile(dir, "profile.json", data)
	if visible {
		result.Publication = VisibleUnconfirmed
		result.Changed = true
	}
	if err != nil {
		return result, err
	}
	result.Publication = ReplicaDurable
	out, err := s.repairOutbox(dir, doc, scope.Members)
	if err != nil {
		return result, err
	}
	result.Outbox = out
	result.Publication = PublicationComplete
	return result, nil
}
func verifyTrusted(doc FleetDocument, trusted func(string) bool) error {
	if err := VerifyFleet(doc); err != nil {
		return err
	}
	if trusted == nil || !trusted(doc.Author) {
		return errors.New("fleet profile author is not a current update administrator")
	}
	return nil
}
func (s *FleetStore) damaged(snapshot Snapshot[FleetDocument]) error {
	if snapshot.State == StateAbsent || snapshot.Revision != nil {
		return nil
	}
	return fmt.Errorf("back up and repair %s before writing; its revision cannot be read safely", filepath.Join(s.dir, "fleet", "profile.json"))
}

// WriteLocal receives a signed N+1 candidate. Only this lock decides whether N
// still holds. The author and editable content identify a retry, not its clock.
func (s *FleetStore) WriteLocal(ctx context.Context, expected uint64, doc FleetDocument, scope FleetScope) (FleetWrite, error) {
	if err := verifyTrusted(doc, scope.Trusted); err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, err
	}
	if _, err := fleetMembers(scope.Members); err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, err
	}
	return s.locked(ctx, func(dir *os.File) (FleetWrite, error) { return s.writeLocal(dir, expected, doc, scope) })
}
func (s *FleetStore) writeLocal(dir *os.File, expected uint64, doc FleetDocument, scope FleetScope) (FleetWrite, error) {
	if err := verifyTrusted(doc, scope.Trusted); err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, err
	}
	snapshot, result, err := s.recover(dir, scope)
	if err != nil {
		return result, err
	}
	if err := s.damaged(snapshot); err != nil {
		return result, err
	}
	revision := *snapshot.Revision
	if snapshot.State == StateValid && expected < math.MaxUint64 && expected+1 == revision && snapshot.Value.Author == doc.Author && snapshot.Value.Fleet == doc.Fleet && sameEditable(snapshot.Value.Editable, doc.Editable) {
		return result, nil
	}
	if expected != revision {
		return result, &ConflictError{Expected: expected, Current: revision}
	}
	if revision == math.MaxUint64 {
		return result, errors.New("fleet revision exhausted; back up and repair fleet/profile.json")
	}
	if doc.Revision != revision+1 {
		return result, invalid(fleetRevisionField, "local candidate must be the expected revision plus one")
	}
	return s.publishDocument(dir, doc, scope)
}

func (s *FleetStore) Receive(ctx context.Context, doc FleetDocument, from string, scope FleetScope) (PutResult, error) {
	if err := VerifyFleet(doc); err != nil {
		return PutResult{}, err
	}
	if scope.Trusted == nil || !scope.Trusted(doc.Author) {
		return PutResult{Outcome: Untrusted}, nil
	}
	if _, err := fleetMembers(scope.Members); err != nil {
		return PutResult{}, err
	}
	var acceptance PutResult
	_, err := s.locked(ctx, func(dir *os.File) (FleetWrite, error) {
		result, reply, err := s.receive(dir, doc, from, scope)
		acceptance = reply
		return result, err
	})
	return acceptance, err
}
func (s *FleetStore) receive(dir *os.File, doc FleetDocument, from string, scope FleetScope) (FleetWrite, PutResult, error) {
	if err := verifyTrusted(doc, scope.Trusted); err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, PutResult{}, err
	}
	snapshot, result, err := s.recover(dir, scope)
	if err != nil {
		return result, PutResult{}, err
	}
	if err := s.damaged(snapshot); err != nil {
		return result, PutResult{}, err
	}
	if snapshot.State != StateAbsent && doc.Revision <= *snapshot.Revision {
		return compareFleet(snapshot, result, doc)
	}
	result, err = s.publishDocument(dir, doc, scope)
	if err != nil {
		return result, PutResult{}, err
	}
	result, err = s.markSender(dir, result, from)
	return result, PutResult{Outcome: Applied, Revision: doc.Revision, Digest: doc.Digest()}, err
}
func compareFleet(snapshot Snapshot[FleetDocument], result FleetWrite, doc FleetDocument) (FleetWrite, PutResult, error) {
	if snapshot.State != StateValid {
		return result, PutResult{}, errors.New("current fleet document is invalid or untrusted; a trusted higher revision is required")
	}
	current := snapshot.Value
	reply := PutResult{Revision: current.Revision, Digest: current.Digest(), Outcome: Conflict}
	if current.Revision > doc.Revision {
		reply.Outcome = Older
		reply.Current = current
	}
	if current.Revision == doc.Revision && current.Digest() == doc.Digest() {
		reply.Outcome = Unchanged
	}
	return result, reply, nil
}
func (s *FleetStore) markSender(dir *os.File, result FleetWrite, from string) (FleetWrite, error) {
	changed := false
	for i := range result.Outbox.Members {
		member := &result.Outbox.Members[i]
		if member.HostID != from || member.State == Delivered {
			continue
		}
		at := s.now().UTC()
		member.State, member.SettledAt, member.LastError = Delivered, &at, nil
		changed = true
	}
	if !changed {
		return result, nil
	}
	if err := s.saveOutbox(dir, result.Outbox); err != nil {
		result.Publication = ReplicaDurable
		return result, err
	}
	return result, nil
}
func (s *FleetStore) Reconcile(ctx context.Context, scope FleetScope) (FleetWrite, error) {
	if _, err := fleetMembers(scope.Members); err != nil {
		return FleetWrite{Publication: BeforeReplicaRename}, err
	}
	return s.locked(ctx, func(dir *os.File) (FleetWrite, error) {
		snapshot, result, err := s.recover(dir, scope)
		if err != nil {
			return result, err
		}
		return result, s.damaged(snapshot)
	})
}
func (s *FleetStore) Requeue(ctx context.Context, scope FleetScope) (FleetWrite, error) {
	return s.locked(ctx, func(dir *os.File) (FleetWrite, error) { return s.requeue(dir, scope) })
}
func (s *FleetStore) requeue(dir *os.File, scope FleetScope) (FleetWrite, error) {
	snapshot, result, err := s.recover(dir, scope)
	if err != nil {
		return result, err
	}
	if err := s.damaged(snapshot); err != nil {
		return result, err
	}
	changed := false
	for i := range result.Outbox.Members {
		member := &result.Outbox.Members[i]
		if member.State != Pending && member.State != Stale {
			continue
		}
		*member = Delivery{HostID: member.HostID, State: Pending, QueuedAt: s.now().UTC()}
		changed = true
	}
	if !changed {
		return result, nil
	}
	if err := s.saveOutbox(dir, result.Outbox); err != nil {
		result.Publication = ReplicaDurable
		return result, err
	}
	result.Publication = PublicationComplete
	return result, nil
}

func (f *EditFile) ReadFleet() ([]byte, error) {
	return readPrivate(f.dir, f.name, FleetLimits.Encoded)
}

func acquireFleetLock(ctx context.Context, dir *os.File) (*os.File, error) {
	file, err := openPrivateFile(dir, ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if errors.Is(err, unix.EEXIST) {
		file, err = openPrivateFile(dir, ".lock", unix.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	if err := waitLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
