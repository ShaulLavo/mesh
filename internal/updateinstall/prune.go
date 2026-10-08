package updateinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var transactionImagePattern = regexp.MustCompile(`^\.mesh-update-[A-Za-z0-9][A-Za-z0-9_-]{0,95}\.(previous|candidate)$`)

// pruneTransactionImages removes rollback and candidate images left beside the
// executable by earlier, finished transactions. Every update names its own pair,
// so without this each update leaves a full copy of the replaced binary behind.
// The files named by current stay. A rollback image that a live session worker
// still executes also stays until that worker exits: macOS can prove a retained
// worker's executable only through a hard link that still names it.
func pruneTransactionImages(dir string, current Status, health Health) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list update images in %s: %w", dir, err)
	}
	keep := map[string]bool{filepath.Base(current.Previous): true, filepath.Base(current.Candidate): true}
	executing, known := workerDigests(health)
	var errs []error
	removed := false
	for _, entry := range entries {
		if keep[entry.Name()] || !transactionImagePattern.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		stale, err := staleImage(path, executing, known)
		if err == nil && stale {
			err = os.Remove(path)
			removed = removed || err == nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if removed {
		errs = append(errs, syncDirectory(dir))
	}
	return errors.Join(errs...)
}

func staleImage(path string, executing map[string]bool, known bool) (bool, error) {
	if !strings.HasSuffix(path, ".previous") {
		return true, nil
	}
	if !known {
		return false, nil
	}
	retained, err := executesImage(path, executing)
	return !retained, err
}

// workerDigests reports false when a worker's executable is unknown, so callers
// can keep every image that worker might be running.
func workerDigests(health Health) (map[string]bool, bool) {
	digests := map[string]bool{}
	for _, worker := range health.Workers {
		if worker.Build == nil || worker.Build.Digest == "" {
			return nil, false
		}
		digests[worker.Build.Digest] = true
	}
	return digests, true
}

func executesImage(path string, digests map[string]bool) (bool, error) {
	if len(digests) == 0 {
		return false, nil
	}
	digest, err := fileDigest(path)
	if err != nil {
		return false, err
	}
	return digests[digest], nil
}
