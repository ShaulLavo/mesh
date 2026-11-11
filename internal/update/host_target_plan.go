package update

import (
	"context"
	"errors"
	"os"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type preparedTarget struct {
	plan    HostTargetPlan
	current release.Build
	source  HostSource
	next    release.PlannedRelease
	proof   hostHopProof
}

type hostTargetRoute struct {
	Schema int                 `json:"schema"`
	Source string              `json:"source"`
	Target string              `json:"target"`
	Cursor release.RouteCursor `json:"cursor"`
}

func (u *HostUpdater) planTarget(ctx context.Context, input HostTargetSubmission, policy HostPolicy, pin *HostTargetRelease, preview bool) (preparedTarget, error) {
	if u.Current == nil || u.Probe == nil || u.Source == nil {
		return preparedTarget{}, errors.New("update: local target verification services unavailable")
	}
	current := u.Current()
	if current.Modified || current.Digest != input.CurrentDigest {
		return preparedTarget{}, errors.New("update: executing source changed or is modified")
	}
	health, err := u.Probe(ctx)
	if err != nil {
		return preparedTarget{}, err
	}
	if health.HostID != u.ID || !sameHostIdentity(health.Build, current) {
		return preparedTarget{}, errors.New("update: observed source differs from executing image")
	}
	current, u.observed = health.Build, health.Build
	source, documents, err := u.Source(ctx, current)
	if err != nil {
		return preparedTarget{}, err
	}
	if err := verifyHostSource(current, source); err != nil {
		return preparedTarget{}, err
	}
	metadata, data, target, err := readLocalTarget(ctx, input.Version, current, source.Epoch, pin, documents)
	if err != nil {
		return preparedTarget{}, err
	}
	limit, err := u.targetOrder(ctx, policy, current, preview)
	if err != nil {
		return preparedTarget{}, err
	}
	u.limit = limit
	budget := release.NewPassBudget()
	selected, selectedData, err := hostLimitMetadata(ctx, source.Epoch, metadata, data, documents, &budget, limit)
	if err != nil {
		return preparedTarget{}, err
	}
	if !newerHostVersion(selected.Version, current.Version) {
		return preparedTarget{}, errors.New("update: predecessors hold the selected target at the current release")
	}
	prepared, err := u.planTargetRoute(ctx, input, current, source, selected, selectedData, documents, limit, health.Workers)
	if err != nil {
		return preparedTarget{}, err
	}
	prepared.plan.PolicyRevision, prepared.plan.CurrentDigest, prepared.plan.Target = policy.Revision, input.CurrentDigest, target
	return prepared, nil
}

func (u *HostUpdater) targetOrder(ctx context.Context, policy HostPolicy, current release.Build, preview bool) (*HostLimit, error) {
	if preview {
		return u.Order.preview(ctx, policy, current, u.now())
	}
	return u.Order.Refresh(ctx, policy, current, u.now())
}

func readLocalTarget(ctx context.Context, version string, current release.Build, epoch uint32, pin *HostTargetRelease, documents release.DocumentSource) (release.UpdateMetadata, []byte, HostTargetRelease, error) {
	metadata, data, err := readTargetMetadata(ctx, version, epoch, pin, documents)
	if err != nil {
		return metadata, nil, HostTargetRelease{}, err
	}
	manifest, err := readTargetManifest(ctx, metadata, documents)
	if err != nil {
		return metadata, nil, HostTargetRelease{}, err
	}
	artifact, err := manifest.Artifact(current.Platform)
	if err != nil {
		return metadata, nil, HostTargetRelease{}, hostError(err)
	}
	target := HostTargetRelease{Version: version, Metadata: discoveryDigest(data), Manifest: metadata.Manifest, Binary: artifact.BinarySHA256}
	if manifest.Version != version || !newerHostVersion(version, current.Version) || artifact.BinarySHA256 == current.Digest {
		return metadata, nil, target, errors.New("update: local target must advance the executing release")
	}
	if pin != nil && *pin != target {
		return metadata, nil, target, errors.New("update: explicitly approved target evidence changed")
	}
	return metadata, data, target, nil
}

func readTargetMetadata(ctx context.Context, version string, epoch uint32, pin *HostTargetRelease, documents release.DocumentSource) (release.UpdateMetadata, []byte, error) {
	ref := release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName}
	if pin != nil {
		ref.SHA256 = pin.Metadata
	}
	data, err := documents.Read(ctx, ref, release.MetadataDocument)
	if err != nil {
		return release.UpdateMetadata{}, nil, hostError(err)
	}
	if len(data) > release.MaxMetadataBytes {
		return release.UpdateMetadata{}, nil, errors.New("update: local target metadata exceeds 64 KiB")
	}
	if ref.SHA256 != "" && discoveryDigest(data) != ref.SHA256 {
		return release.UpdateMetadata{}, nil, errors.New("update: approved target metadata pin changed")
	}
	ref.SHA256 = discoveryDigest(data)
	metadata, err := release.AuthenticateMetadata(ctx, documents, ref, data, epoch)
	if err != nil {
		return metadata, nil, hostError(err)
	}
	if metadata.Updater == nil {
		return metadata, nil, errors.New("update: local target lacks authenticated updater capability")
	}
	return metadata, data, nil
}

func readTargetManifest(ctx context.Context, metadata release.UpdateMetadata, documents release.DocumentSource) (release.Manifest, error) {
	budget := release.NewPassBudget()
	manifestData, err := release.ReadDocument(ctx, documents, release.AssetRef{Tag: metadata.Version, Name: release.ManifestAssetName, SHA256: metadata.Manifest}, release.ManifestDocument, &budget)
	if err != nil {
		return release.Manifest{}, hostError(err)
	}
	if _, err := release.ReadDocument(ctx, documents, metadata.Page, release.PageDocument, &budget); err != nil {
		return release.Manifest{}, hostError(err)
	}
	var manifest release.Manifest
	if err := decodeAuthority(manifestData, &manifest); err != nil {
		return manifest, err
	}
	if err := manifest.Validate(); err != nil {
		return manifest, hostError(err)
	}
	return manifest, nil
}

func (u *HostUpdater) planTargetRoute(ctx context.Context, input HostTargetSubmission, current release.Build, source HostSource, target release.UpdateMetadata, data []byte, documents release.DocumentSource, limit *HostLimit, workers []updateinstall.Worker) (preparedTarget, error) {
	cache := hostTargetRoute{Schema: 1, Source: current.Digest, Target: discoveryDigest(data)}
	var saved hostTargetRoute
	if err := u.Store.readAuthority("host-target-route.json", &saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return preparedTarget{}, err
	}
	if saved.Schema == 1 && saved.Source == cache.Source && saved.Target == cache.Target {
		cache.Cursor = saved.Cursor
	}
	cursor := cache.Cursor
	cursor.Failed = targetRouteFailures(cursor.Failed, input.RetryEdge)
	if input.RetryEdge != nil {
		cursor.Search = nil
	}
	for range 4 {
		prepared, nextCursor, failed, err := u.targetRoutePass(ctx, input, current, source, target, cursor, data, documents, limit, workers)
		cache.Cursor = nextCursor
		if saveErr := u.saveTargetRoute(ctx, cache); saveErr != nil {
			return preparedTarget{}, saveErr
		}
		if err != nil {
			return preparedTarget{}, err
		}
		if !failed {
			return prepared, nil
		}
		cursor = nextCursor
	}
	return preparedTarget{}, errors.New("update: failed edges hold the route; verified search will continue on the next check")
}

func (u *HostUpdater) targetRoutePass(ctx context.Context, input HostTargetSubmission, current release.Build, source HostSource, target release.UpdateMetadata, cursor release.RouteCursor, data []byte, documents release.DocumentSource, limit *HostLimit, workers []updateinstall.Worker) (preparedTarget, release.RouteCursor, bool, error) {
	step, err := release.NextHop(ctx, current, source.Position, target, cursor, documents, release.NewPassBudget())
	if err != nil {
		return preparedTarget{}, cursor, false, hostError(err)
	}
	cursor = step.Cursor
	if step.Next == nil {
		return preparedTarget{}, cursor, false, errors.New("update: verified route is incomplete or unavailable; retry the local target check")
	}
	prepared, failed, err := u.checkTargetHop(ctx, input, current, source, *step.Next, cursor, data, documents, limit, workers)
	if failed {
		cursor.Failed = []release.FailedEdge{{FromDigest: current.Digest, ToDigest: prepared.plan.Next.Binary, Platform: current.Platform}}
	}
	return prepared, cursor, failed, err
}

func targetRouteFailures(failed []release.FailedEdge, retry *Edge) []release.FailedEdge {
	if retry == nil {
		return failed
	}
	kept := failed[:0]
	for _, entry := range failed {
		if entry.FromDigest != retry.From || entry.ToDigest != retry.To || entry.Platform != retry.Platform {
			kept = append(kept, entry)
		}
	}
	return kept
}

func (u *HostUpdater) checkTargetHop(ctx context.Context, input HostTargetSubmission, current release.Build, source HostSource, next release.PlannedRelease, cursor release.RouteCursor, data []byte, documents release.DocumentSource, limit *HostLimit, workers []updateinstall.Worker) (preparedTarget, bool, error) {
	artifact, err := next.Manifest.Artifact(current.Platform)
	if err != nil {
		return preparedTarget{}, false, hostError(err)
	}
	edge := Edge{From: current.Digest, To: artifact.BinarySHA256, Platform: current.Platform}
	failed, err := u.Store.failedHostEdge(edge)
	if err != nil {
		return preparedTarget{}, false, err
	}
	result := preparedTarget{current: current, source: source, next: next, plan: HostTargetPlan{Next: HostTargetRelease{Version: next.Manifest.Version, Binary: artifact.BinarySHA256}}}
	retrying := input.RetryEdge != nil && *input.RetryEdge == edge
	if failed && !retrying || limit.failed(edge) {
		return result, true, nil
	}
	if cursor.Search != nil && cursor.Search.Pending != nil && len(cursor.Search.Pending.Metadata) > 0 {
		data = cursor.Search.Pending.Metadata
	}
	proof, err := verifyHostHop(ctx, current, workers, next, data, documents)
	if err != nil {
		return result, false, err
	}
	proof.Source = source.Manifest
	result.proof = proof
	result.plan.Next.Metadata, result.plan.Next.Manifest, result.plan.Next.Forward = proof.Metadata, next.Metadata.Manifest, next.Transition.Proof
	for _, rollback := range next.Metadata.Rollback {
		if rollback.Forward == next.Transition.Proof {
			result.plan.Next.Rollback = rollback.Rollback
		}
	}
	return result, false, nil
}

func (u *HostUpdater) saveTargetRoute(ctx context.Context, cache hostTargetRoute) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return u.Store.writeAuthority("host-target-route.json", cache)
}
