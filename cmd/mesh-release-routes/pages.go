package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/release"
)

type retainedPage struct {
	Page release.AnchorPage
	Ref  release.AssetRef
}

func anchorPrefix(prior, entries []release.AnchorEntry) bool {
	return len(prior) <= len(entries) && slices.Equal(prior, entries[:len(prior)])
}

func reusePage(retained retainedPage, entries []release.AnchorEntry) (release.AssetRef, bool, error) {
	if retained.Ref.Tag == "" {
		return retained.Ref, false, nil
	}
	if !anchorPrefix(retained.Page.Entries, entries) {
		return retained.Ref, false, errors.New("publisher: published anchor page entries changed")
	}
	return retained.Ref, len(retained.Page.Entries) == len(entries), nil
}

func loadPriorPages(directory string, history []publishedEvidence, epoch uint32) (map[uint64]retainedPage, error) {
	pages := map[uint64]retainedPage{}
	for _, item := range slices.Backward(history) {
		if item.Metadata == nil || item.Metadata.Epoch != epoch {
			continue
		}
		if err := loadPageChain(directory, item.Metadata.Page, pages); err != nil {
			return nil, err
		}
		break
	}
	return pages, nil
}

func loadPageChain(directory string, first release.AssetRef, pages map[uint64]retainedPage) error {
	pending := []release.AssetRef{first}
	seen := map[release.AssetRef]bool{}
	for len(pending) > 0 {
		ref := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		back, err := retainPage(directory, ref, pages)
		if err != nil {
			return err
		}
		pending = append(pending, back...)
	}
	return nil
}

func retainPage(directory string, ref release.AssetRef, pages map[uint64]retainedPage) ([]release.AssetRef, error) {
	data, err := readAsset(directory, ref, release.MaxAnchorPageBytes)
	if err != nil {
		return nil, err
	}
	page, err := release.DecodeAnchorPage(data, ref)
	if err != nil {
		return nil, fmt.Errorf("publisher: retained page: %w", err)
	}
	if prior, exists := pages[page.Page]; exists && prior.Ref != ref {
		return nil, errors.New("publisher: retained page references disagree")
	}
	pages[page.Page] = retainedPage{Ref: ref, Page: page}
	back := make([]release.AssetRef, len(page.Back))
	for index, item := range page.Back {
		back[index] = item.Ref
	}
	return back, nil
}
