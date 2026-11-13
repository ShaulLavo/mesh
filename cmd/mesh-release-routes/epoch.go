package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/release"
)

func validateEpoch(directory string, input publicationInput, history []publishedEvidence) error {
	newest := newestEpoch(history)
	if input.Epoch < newest || input.Epoch-newest > 1 {
		return errors.New("publisher: format epochs must advance one at a time")
	}
	if input.Epoch == 1 {
		return validateFirstEpoch(input)
	}
	if input.PreviousRoot == nil {
		return errors.New("publisher: a newer epoch requires its prior authenticated root")
	}
	data, err := readAsset(directory, *input.PreviousRoot, release.MaxDiscoveryRootBytes)
	if err != nil {
		return err
	}
	root, err := release.DecodeDiscoveryRoot(data)
	if err != nil {
		return fmt.Errorf("publisher: prior epoch root: %w", err)
	}
	if root.Epoch != input.Epoch-1 || root.Head.Tag != input.Waypoint || root.Head.Tag != input.PreviousRoot.Tag {
		return errors.New("publisher: previous root does not name the prior epoch waypoint")
	}
	return validateWaypoint(input, history, root)
}

func newestEpoch(history []publishedEvidence) uint32 {
	for _, item := range slices.Backward(history) {
		if item.Metadata != nil {
			return item.Metadata.Epoch
		}
	}
	return 1
}

func validateFirstEpoch(input publicationInput) error {
	if input.PreviousRoot != nil || input.Waypoint != "" {
		return errors.New("publisher: the first epoch has no prior waypoint")
	}
	return nil
}

func validateWaypoint(input publicationInput, history []publishedEvidence, root release.DiscoveryRoot) error {
	index := slices.IndexFunc(history, func(item publishedEvidence) bool { return item.Tag == input.Waypoint })
	if index < 0 || history[index].Metadata == nil {
		return errors.New("publisher: waypoint lacks its old-format metadata")
	}
	waypoint := history[index]
	if sum(waypoint.MetadataBytes) != root.Head.SHA256 || waypoint.Metadata.Epoch != root.Epoch || !waypoint.Metadata.Position.Anchor {
		return errors.New("publisher: waypoint must be a bound anchor written in the old epoch")
	}
	return nil
}
