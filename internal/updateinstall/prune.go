package updateinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var transactionImagePattern = regexp.MustCompile(`^\.mesh-update-[A-Za-z0-9][A-Za-z0-9_-]{0,95}\.(previous|candidate)$`)

// macOS proves a retained worker's executable through lsof, which resolves a
// loaded vnode only while some hard link still names it. Linux reads
// /proc/<pid>/exe, which works after every link is gone.
var workerImagesNeedLinks = runtime.GOOS == "darwin"

// pruneTransactionImages removes rollback and candidate images left beside the
// executable by earlier, finished transactions. Every update names its own pair,
// so without this each update leaves a full copy of the replaced binary behind.
// The files named by current stay. On macOS, a rollback image that a live session
// worker still executes also stays until that worker exits.
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
	if !strings.HasSuffix(path, ".previous") || !workerImagesNeedLinks {
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
