package updateinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var helperDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Called with the installation lock held, after a successful helper operation.
func pruneHelperImages(stateDir, installedDigest string, previous ...string) error {
	dir := filepath.Join(transactionDir(stateDir), "helper")
	keep, err := retainedHelperDigests(stateDir, dir, installedDigest)
	if err != nil {
		return err
	}
	for _, digest := range previous {
		if helperDigestPattern.MatchString(digest) && digest != installedDigest {
			keep[digest] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list helper images: %w", err)
	}
	var obsolete []os.FileInfo
	for _, entry := range entries {
		if !entry.IsDir() || !helperDigestPattern.MatchString(entry.Name()) || keep[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect helper image: %w", err)
		}
		obsolete = append(obsolete, info)
	}
	// Keep the most recent prior helper available for recovery as well.
	sort.Slice(obsolete, func(i, j int) bool { return obsolete[i].ModTime().After(obsolete[j].ModTime()) })
	var errs []error
	for _, info := range obsolete[min(1, len(obsolete)):] {
		errs = append(errs, removeHelperImage(dir, info.Name()))
	}
	return errors.Join(errs...)
}

func retainedHelperDigests(stateDir, dir, installedDigest string) (map[string]bool, error) {
	keep := map[string]bool{installedDigest: true}
	link, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		return nil, fmt.Errorf("read current helper link: %w", err)
	}
	keep[filepath.Base(filepath.Dir(link))] = true
	pending, err := os.ReadFile(helperActivationRecord(stateDir)) //nolint:gosec // fixed helper metadata path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read pending helper activation: %w", err)
	}
	if err == nil && !helperDigestPattern.Match(pending) {
		return nil, errors.New("invalid pending helper digest")
	}
	keep[string(pending)] = true
	status, err := Read(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	keep[status.Request.Current.Digest] = true
	// A helper can still execute its old image while its replacement starts.
	if executable, err := os.Executable(); err == nil && within(dir, executable) {
		keep[filepath.Base(filepath.Dir(executable))] = true
	}
	return keep, nil
}

func removeHelperImage(dir, digest string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open helper images: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(digest); err != nil {
		return fmt.Errorf("remove obsolete helper image: %w", err)
	}
	return nil
}
