package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

type HostPeer struct {
	ID          string            `json:"id"`
	AfterSeq    uint64            `json:"afterSeq"`
	FailedAfter string            `json:"failedAfter,omitempty"`
	State       EvidenceReadState `json:"state"`
	ReadAt      time.Time         `json:"readAt"`
	Problem     string            `json:"problem,omitempty"`
	Held        bool              `json:"held"`
}

type hostPeers struct {
	Schema  int        `json:"schema"`
	Entries []HostPeer `json:"entries"`
}

type HostLimit struct {
	Version  string
	Metadata string
	Failed   []FailedEdge
}

// HostOrder is wired to the daemon's authenticated device client and saved
// identity fleet. Peer history lives in individual bounded authority records.
type HostOrder struct {
	Store      *Store
	Client     Client
	SavedFleet func() (Fleet, error)
}

func (o *HostOrder) Refresh(ctx context.Context, policy HostPolicy, current release.Build, now time.Time) (*HostLimit, error) {
	return o.refresh(ctx, policy, current, now, true)
}

func (o *HostOrder) preview(ctx context.Context, policy HostPolicy, current release.Build, now time.Time) (*HostLimit, error) {
	return o.refresh(ctx, policy, current, now, false)
}

func (o *HostOrder) refresh(ctx context.Context, policy HostPolicy, current release.Build, now time.Time, acknowledge bool) (*HostLimit, error) {
	if len(policy.After) == 0 {
		return nil, nil
	}
	if o == nil || o.Store == nil || o.SavedFleet == nil {
		return nil, errors.New("update: predecessor reader unavailable")
	}
	fleet, err := o.SavedFleet()
	if err != nil {
		return nil, hostError(err)
	}
	var result *HostLimit
	settled := ""
	if acknowledge {
		settled = current.Version
	}
	for _, id := range policy.After {
		peer := findEvidenceHost(fleet, id)
		if err := o.refreshPeer(ctx, id, peer, settled, now); err != nil {
			return nil, err
		}
		limit, err := o.Store.peerLimit(ctx, id, current, policy.Soak, now)
		if err != nil {
			return nil, err
		}
		if err := o.Store.prunePeerHistory(ctx, id, current, policy.Soak, now); err != nil {
			return nil, err
		}
		result, err = minimumHostLimit(result, limit)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func findEvidenceHost(fleet Fleet, id string) *Host {
	for _, host := range fleet.Members {
		if host.ID == id {
			return &host
		}
	}
	return nil
}

func minimumHostLimit(result, limit *HostLimit) (*HostLimit, error) {
	if result == nil {
		return limit, nil
	}
	order, err := release.CompareVersions(result.Version, limit.Version)
	if err != nil {
		return nil, hostError(err)
	}
	result.Failed = append(result.Failed, limit.Failed...)
	failed := result.Failed
	if order == 0 && result.Metadata != limit.Metadata {
		return nil, errors.New("update: predecessors disagree on exact release metadata")
	}
	if order > 0 {
		result = limit
	}
	result.Failed = failed
	return result, nil
}

func (o *HostOrder) refreshPeer(ctx context.Context, id string, host *Host, settled string, now time.Time) error {
	peer, err := o.Store.hostPeer(ctx, id)
	if err != nil {
		return err
	}
	if host == nil {
		return o.Store.recordPeerRead(ctx, peer, now, evidenceError(EvidenceInvalid, errors.New("predecessor identity is absent from saved fleet")))
	}
	request := EvidenceRequest{Settled: settled, AfterSeq: peer.AfterSeq, Limit: 64, FailedAfter: peer.FailedAfter}
	// Each tick has bounded work. More pages resume from durable sequence state.
	for range 4 {
		reply, readErr := o.Client.Evidence(ctx, *host, request)
		if readErr != nil {
			return o.Store.recordPeerRead(ctx, peer, now, readErr)
		}
		if err := o.Store.savePeerPage(ctx, id, request, reply); err != nil {
			return o.Store.recordPeerRead(ctx, peer, now, evidenceError(EvidenceInvalid, err))
		}
		if len(reply.Entries) > 0 {
			request.AfterSeq = reply.Entries[len(reply.Entries)-1].Seq
		}
		request.FailedAfter = reply.FailedNext
		peer.AfterSeq = request.AfterSeq
		if !reply.More && reply.FailedNext == "" {
			return o.Store.recordPeerRead(ctx, peer, now, nil)
		}
	}
	return o.Store.recordPeerRead(ctx, peer, now, evidenceError(EvidenceReading, errors.New("predecessor evidence has more pages; continuing next pass")))
}

func (s *Store) readHostPeers() (hostPeers, error) {
	peers := hostPeers{Schema: 1}
	err := s.readAuthority("host-peers.json", &peers)
	if errors.Is(err, os.ErrNotExist) {
		return peers, nil
	}
	if err != nil {
		return peers, err
	}
	if peers.Schema != 1 || len(peers.Entries) > MaximumHosts {
		return peers, errors.New("update: invalid peer evidence index")
	}
	seen := map[string]bool{}
	for _, peer := range peers.Entries {
		if _, err := PublicKey(peer.ID); err != nil {
			return peers, err
		}
		if err := validHostPeer(peer); err != nil {
			return peers, err
		}
		if seen[peer.ID] {
			return peers, errors.New("update: duplicate predecessor evidence")
		}
		seen[peer.ID] = true
	}
	return peers, nil
}

func (s *Store) HostPeers(ctx context.Context) ([]HostPeer, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	peers, err := s.readHostPeers()
	return peers.Entries, err
}

func (s *Store) hostPeer(ctx context.Context, id string) (HostPeer, error) {
	peers, err := s.HostPeers(ctx)
	if err != nil {
		return HostPeer{}, err
	}
	for _, peer := range peers {
		if peer.ID == id {
			return peer, nil
		}
	}
	return HostPeer{ID: id, Held: true}, nil
}

func (s *Store) putHostPeer(peer HostPeer) error {
	peers, err := s.readHostPeers()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(peers.Entries, func(value HostPeer) bool { return value.ID == peer.ID })
	if index >= 0 {
		peers.Entries[index] = peer
	} else {
		peers.Entries = append(peers.Entries, peer)
	}
	if len(peers.Entries) > MaximumHosts {
		return errors.New("update: peer evidence capacity reached")
	}
	return s.writeAuthority("host-peers.json", peers)
}

func (s *Store) recordPeerRead(ctx context.Context, peer HostPeer, now time.Time, cause error) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	prior, err := s.readHostPeers()
	if err != nil {
		return err
	}
	for _, saved := range prior.Entries {
		if saved.ID == peer.ID {
			peer = saved
			break
		}
	}
	peer.ReadAt, peer.State, peer.Problem = now, EvidenceVerified, ""
	if cause == nil {
		peer.Held = false
		return s.putHostPeer(peer)
	}
	var readErr *EvidenceReadError
	peer.State = EvidenceInvalid
	if errors.As(cause, &readErr) {
		peer.State = readErr.State
	}
	peer.Held = peer.Held || peer.State != EvidenceOffline
	peer.Problem = boundedProblem(cause.Error())
	return s.putHostPeer(peer)
}

func peerEntryName(id string, seq uint64) string {
	return fmt.Sprintf("host-peer-%s-%020d.json", id, seq)
}
func peerFailureName(id string, edge Edge) string {
	return "host-peer-failure-" + id + "-" + discoveryDigest([]byte(failedEdgeName(edge))) + ".json"
}

func (s *Store) savePeerPage(ctx context.Context, id string, request EvidenceRequest, reply EvidenceReply) error {
	if err := validateEvidenceReply(id, request, reply); err != nil {
		return err
	}
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if reply.Phase == HostAttention || reply.Attention != "" {
		return fmt.Errorf("update: predecessor %s requires attention: %s", id, reply.Attention)
	}
	for _, entry := range reply.Entries {
		if err := s.savePeerEntry(id, entry); err != nil {
			return err
		}
	}
	for _, failed := range reply.Failed {
		if err := s.writeAuthority(peerFailureName(id, failed.Edge), failed); err != nil {
			return err
		}
	}
	peers, err := s.readHostPeers()
	if err != nil {
		return err
	}
	peer := HostPeer{ID: id, Held: true}
	for _, saved := range peers.Entries {
		if saved.ID == id {
			peer = saved
			break
		}
	}
	if len(reply.Entries) > 0 {
		peer.AfterSeq = reply.Entries[len(reply.Entries)-1].Seq
	}
	peer.FailedAfter = reply.FailedNext
	return s.putHostPeer(peer)
}

func (s *Store) savePeerEntry(id string, entry LedgerEntry) error {
	name := peerEntryName(id, entry.Seq)
	var prior LedgerEntry
	err := s.readAuthority(name, &prior)
	if err == nil && !reflect.DeepEqual(prior, entry) {
		return errors.New("update: predecessor changed a committed sequence")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.writeAuthority(name, entry)
}

func (s *Store) peerLimit(ctx context.Context, id string, current release.Build, soak time.Duration, now time.Time) (*HostLimit, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.savedPeerLimit(id, current, soak, now)
}

func (s *Store) savedPeerLimit(id string, current release.Build, soak time.Duration, now time.Time) (*HostLimit, error) {
	peers, err := s.readHostPeers()
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(peers.Entries, func(peer HostPeer) bool { return peer.ID == id })
	if index < 0 {
		return nil, fmt.Errorf("update: predecessor %s has no verified evidence", id)
	}
	peer := peers.Entries[index]
	if peer.Held {
		return nil, fmt.Errorf("update: predecessor %s held (%s): %s", id, peer.State, peer.Problem)
	}
	names, err := s.peerRecordNames(id)
	if err != nil {
		return nil, err
	}
	result := &HostLimit{}
	for _, name := range names {
		if err := s.includePeerRecord(name, current, soak, now, result); err != nil {
			return nil, err
		}
	}
	if result.Version == "" {
		return nil, fmt.Errorf("update: predecessor %s has no soaked committed release", id)
	}
	return result, nil
}

func (s *Store) peerRecordNames(id string) ([]string, error) {
	dir, err := s.authorityDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, hostError(err)
	}
	return slices.DeleteFunc(names, func(name string) bool {
		return !strings.HasPrefix(name, "host-peer-"+id+"-") && !strings.HasPrefix(name, "host-peer-failure-"+id+"-")
	}), nil
}

func (s *Store) includePeerRecord(name string, current release.Build, soak time.Duration, now time.Time, result *HostLimit) error {
	if strings.HasPrefix(name, "host-peer-failure-") {
		return s.includePeerFailure(name, current, result)
	}
	var entry LedgerEntry
	if err := s.readAuthority(name, &entry); err != nil {
		return err
	}
	if err := validEvidenceEntry(entry); err != nil {
		return err
	}
	if entry.Metadata == "" || entry.WorkersPreserved != entry.Workers {
		return nil
	}
	if entry.CommittedAt.After(now.Add(-soak)) {
		return nil
	}
	if result.Version == "" {
		result.Version, result.Metadata = entry.Version, entry.Metadata
		return nil
	}
	order, err := release.CompareVersions(entry.Version, result.Version)
	if err != nil {
		return hostError(err)
	}
	if order == 0 && entry.Metadata != result.Metadata {
		return errors.New("update: predecessor changed exact release metadata pin")
	}
	if order > 0 {
		result.Version, result.Metadata = entry.Version, entry.Metadata
	}
	return nil
}

func (limit *HostLimit) allows(version string) bool {
	if limit == nil {
		return true
	}
	order, err := release.CompareVersions(version, limit.Version)
	return err == nil && order <= 0
}

func (limit *HostLimit) failed(edge Edge) bool {
	if limit == nil {
		return false
	}
	return slices.ContainsFunc(limit.Failed, func(failed FailedEdge) bool { return failed.Edge == edge })
}

// DecodeEvidence bounds original control bytes, including opaque details.
func DecodeEvidence(data json.RawMessage, destination any) error {
	return decodeMessage(data, destination)
}

func newerHostVersion(a, b string) bool {
	order, err := release.CompareVersions(a, b)
	return err == nil && order > 0
}

func hostLimitMetadata(ctx context.Context, epoch uint32, head release.UpdateMetadata, original []byte, documents release.DocumentSource, budget *release.PassBudget, limit *HostLimit) (release.UpdateMetadata, []byte, error) {
	if limit == nil {
		return head, original, nil
	}
	order, err := release.CompareVersions(head.Version, limit.Version)
	if err != nil {
		return head, nil, hostError(err)
	}
	// A newer predecessor may use a future format. Its bytes are never decoded
	// while this reader is following its own epoch's head and waypoints.
	if order < 0 {
		return head, original, nil
	}
	if order == 0 {
		if discoveryDigest(original) != limit.Metadata {
			return head, nil, errors.New("update: predecessor pin differs from authenticated discovery head")
		}
		return head, original, nil
	}
	ref := release.AssetRef{Tag: limit.Version, Name: release.UpdateMetadataAssetName, SHA256: limit.Metadata}
	data, err := release.ReadDocument(ctx, documents, ref, release.MetadataDocument, budget)
	if err != nil {
		return head, nil, hostError(err)
	}
	metadata, err := release.AuthenticateMetadata(ctx, documents, ref, data, epoch)
	return metadata, data, hostError(err)
}

// Called under the existing installation -> authority acceptance lock order.
func (s *Store) checkSavedHostOrder(request updateinstall.Request, now time.Time) error {
	policy, err := s.hostPolicy()
	if err != nil {
		return err
	}
	artifact, err := request.Manifest.Artifact(request.Current.Platform)
	if err != nil {
		return hostError(err)
	}
	edge := Edge{From: request.Current.Digest, To: artifact.BinarySHA256, Platform: request.Current.Platform}
	for _, id := range policy.After {
		limit, err := s.savedPeerLimit(id, request.Current, policy.Soak, now)
		if err != nil {
			return err
		}
		if !limit.allows(request.Manifest.Version) || limit.failed(edge) {
			return errors.New("update: predecessor evidence withdrew hop permission")
		}
	}
	return nil
}

func (o *HostOrder) ReleaseDropped(ctx context.Context, policy HostPolicy) error {
	if o == nil || o.Store == nil {
		return nil
	}
	peers, err := o.Store.HostPeers(ctx)
	if err != nil {
		return err
	}
	dropped := slices.DeleteFunc(peers, func(peer HostPeer) bool { return slices.Contains(policy.After, peer.ID) })
	if len(dropped) == 0 {
		return nil
	}
	if o.SavedFleet == nil {
		return errors.New("update: cannot release predecessors without identity fleet")
	}
	fleet, err := o.SavedFleet()
	if err != nil {
		return hostError(err)
	}
	for _, peer := range dropped {
		if err := o.releasePeer(ctx, fleet, peer); err != nil {
			return err
		}
	}
	return nil
}

func (o *HostOrder) releasePeer(ctx context.Context, fleet Fleet, peer HostPeer) error {
	host := findEvidenceHost(fleet, peer.ID)
	if host == nil {
		return fmt.Errorf("update: dropped predecessor %s needs its saved endpoint to release retention", peer.ID)
	}
	_, err := o.Client.Evidence(ctx, *host, EvidenceRequest{Drop: true, Limit: 1})
	if err != nil {
		return err
	}
	return o.Store.removeHostPeer(ctx, peer.ID)
}

func (s *Store) removeHostPeer(ctx context.Context, id string) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	peers, err := s.readHostPeers()
	if err != nil {
		return err
	}
	peers.Entries = slices.DeleteFunc(peers.Entries, func(peer HostPeer) bool { return peer.ID == id })
	names, err := s.peerRecordNames(id)
	if err != nil {
		return err
	}
	if err := s.removePeerRecords(names); err != nil {
		return err
	}
	return s.writeAuthority("host-peers.json", peers)
}

func (s *Store) removePeerRecords(names []string) error {
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for _, name := range names {
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("update: prune peer evidence: %w", err)
		}
	}
	return syncHostAuthority(dir)
}

func (s *Store) prunePeerHistory(ctx context.Context, id string, current release.Build, soak time.Duration, now time.Time) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	names, err := s.peerRecordNames(id)
	if err != nil {
		return err
	}
	var entries []LedgerEntry
	var keep LedgerEntry
	for _, name := range names {
		if strings.HasPrefix(name, "host-peer-failure-") {
			continue
		}
		var entry LedgerEntry
		if err := s.readAuthority(name, &entry); err != nil {
			return err
		}
		entries = append(entries, entry)
		if entry.Metadata != "" && entry.Workers == entry.WorkersPreserved && !entry.CommittedAt.After(now.Add(-soak)) && (keep.Seq == 0 || newerHostVersion(entry.Version, keep.Version)) {
			keep = entry
		}
	}
	var remove []string
	for _, entry := range entries {
		if entry.Seq != keep.Seq && !newerHostVersion(entry.Version, current.Version) {
			remove = append(remove, peerEntryName(id, entry.Seq))
		}
	}
	return s.removePeerRecords(remove)
}

func validHostPeer(peer HostPeer) error {
	if len(peer.Problem) > 512 || len(peer.FailedAfter) > 256 || stringsContainsPath(peer.FailedAfter) {
		return errors.New("update: invalid saved peer bounds")
	}
	switch peer.State {
	case EvidenceVerified, EvidenceOffline:
		if peer.ReadAt.IsZero() && !peer.Held {
			return errors.New("update: peer evidence lacks verified read time")
		}
	case "", EvidenceReading, EvidenceRefused, EvidenceUnsupported, EvidenceInvalid:
		if !peer.Held {
			return errors.New("update: failed peer evidence lacks hold")
		}
	default:
		return errors.New("update: invalid saved peer read state")
	}
	return nil
}

func (s *Store) includePeerFailure(name string, current release.Build, result *HostLimit) error {
	var failed FailedEdge
	if err := s.readAuthority(name, &failed); err != nil {
		return err
	}
	if err := validEvidenceFailure(failed); err != nil {
		return err
	}
	if failed.Edge.Platform == current.Platform && failed.Edge.From == current.Digest {
		result.Failed = append(result.Failed, failed)
	}
	return nil
}

func (s *Store) commitHostOrder(request updateinstall.Request, now time.Time, commit func() error) error {
	if err := s.checkSavedHostOrder(request, now); err != nil {
		return err
	}
	return commit()
}

func (s *Store) retainHostRouteFailures(failed []release.FailedEdge, limit *HostLimit) ([]release.FailedEdge, error) {
	retained := failed[:0]
	for _, entry := range failed {
		edge := Edge{From: entry.FromDigest, To: entry.ToDigest, Platform: entry.Platform}
		local, err := s.failedHostEdge(edge)
		if err != nil {
			return nil, err
		}
		if local || limit.failed(edge) {
			retained = append(retained, entry)
		}
	}
	return retained, nil
}
