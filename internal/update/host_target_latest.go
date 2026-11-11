package update

import (
	"context"
	"errors"
	"os"

	"github.com/shaul/mesh/internal/release"
)

func (u *HostUpdater) readTargetSelection(ctx context.Context, version string, current release.Build, epoch uint32, pin *HostTargetRelease, documents release.DocumentSource, preview bool) (release.UpdateMetadata, []byte, HostTargetRelease, error) {
	if version != "latest" || !preview {
		return readLocalTarget(ctx, version, current, epoch, pin, documents, preview)
	}
	var cursor DiscoveryCursor
	if err := u.Store.readAuthority("host-target-latest.json", &cursor); err != nil && !errors.Is(err, os.ErrNotExist) {
		return release.UpdateMetadata{}, nil, HostTargetRelease{}, err
	}
	step := DiscoveryStep{Cursor: cursor, Budget: release.NewPassBudget()}
	metadata, err := previewLatestMetadata(ctx, epoch, &step, documents)
	if saveErr := u.saveTargetLatest(ctx, step.Cursor); saveErr != nil {
		return metadata, nil, HostTargetRelease{}, saveErr
	}
	if err != nil {
		return metadata, nil, HostTargetRelease{}, err
	}
	target, err := targetReleaseEvidence(ctx, metadata, step.Cursor.Metadata, current, nil, documents, true)
	return metadata, step.Cursor.Metadata, target, err
}

func previewLatestMetadata(ctx context.Context, epoch uint32, step *DiscoveryStep, documents release.DocumentSource) (release.UpdateMetadata, error) {
	if err := discoverHead(ctx, epoch, step, documents); err != nil {
		return release.UpdateMetadata{}, err
	}
	metadata, err := discoveryMetadata(ctx, epoch, step, documents)
	if err != nil {
		return metadata, err
	}
	if metadata.Updater == nil {
		return metadata, errors.New("update: compatible discovery head lacks authenticated updater capability")
	}
	return metadata, nil
}

func (u *HostUpdater) saveTargetLatest(ctx context.Context, cursor DiscoveryCursor) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return u.Store.writeAuthority("host-target-latest.json", cursor)
}
