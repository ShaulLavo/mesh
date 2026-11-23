// Package execution owns durable keyed-command records, below worker and daemon.
package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/durable"
)

const MaxRecord = 64 << 10

// maxConfirmed bounds the confirmation memory of one Files. It is a variable
// only so tests can reach the bound without publishing thousands of records.
var maxConfirmed = 512

// MaxBoundedRecord is the largest record PublishBounded accepts.
const MaxBoundedRecord = 256 << 10

type Stage string

const (
	BeforePublish Stage = "before_publish"
	Unconfirmed   Stage = "visible_unconfirmed"
	Durable       Stage = "durable"
)

type PersistenceError struct {
	Stage Stage
	Err   error
}

func (e *PersistenceError) Error() string {
	return fmt.Sprintf("execution persistence %s: %v", e.Stage, e.Err)
}
func (e *PersistenceError) Unwrap() error { return e.Err }

// Files anchors all paths to the existing state root. No caller path is accepted.
type Files struct {
	root  *os.Root
	fault func(string) error
	// observe sees every sync step without changing behavior, so tests can
	// count them with the confirmation memory still active.
	observe func(string)
	hookMu  sync.Mutex
	// faultKeepsMemory lets a test inject faults with the confirmation memory
	// still active, to check what a failed barrier leaves remembered.
	faultKeepsMemory bool
	// confirmed remembers JSON records this process already proved durable, so
	// an unchanged one skips its file fsync. Directory and device barriers are
	// never skipped: a rename can be undone or repeated without touching the
	// file, so only the file's own bytes are vouched for here. It is bounded and
	// forgets a path when the path is removed.
	confirmMu sync.Mutex
	confirmed map[string]fileStamp
}

func OpenFiles(path string) (*Files, error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, executionBoundaryError("open execution parent", err)
	}
	name := filepath.Base(path)
	if name == string(filepath.Separator) {
		name = "."
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, errors.Join(executionBoundaryError("inspect execution root", err), parent.Close())
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(fmt.Errorf("execution: unsafe state root"), parent.Close())
	}
	root, err := parent.OpenRoot(name)
	err = errors.Join(err, parent.Close())
	if err != nil {
		if root != nil {
			err = errors.Join(err, root.Close())
		}
		return nil, executionBoundaryError("open execution root", err)
	}
	return &Files{root: root, confirmed: make(map[string]fileStamp)}, nil
}

func (f *Files) Close() error { return executionBoundaryError("files.go Close", f.root.Close()) }

func validPath(name string) bool {
	return filepath.IsLocal(name) && filepath.Clean(name) == name && name != "."
}
func (f *Files) check(name string, absent bool) error {
	if !validPath(name) {
		return fmt.Errorf("execution: invalid private path")
	}
	parts := strings.Split(name, string(filepath.Separator))
	for i := range parts {
		current := filepath.Join(parts[:i+1]...)
		info, err := f.root.Lstat(current)
		if absent && i == len(parts)-1 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return executionBoundaryError("files.go check", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("execution: symlink at %s", current)
		}
		if i != len(parts)-1 {
			continue
		}
		if err := unlinkedRecord(info); err != nil {
			return err
		}
	}
	return nil
}
func regular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return executionBoundaryError("files.go regular", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || st.Nlink != 1 {
		return fmt.Errorf("execution: non-private regular file")
	}
	return nil
}
func (f *Files) open(name string, flags int) (*os.File, error) {
	if err := f.check(name, flags&os.O_CREATE != 0); err != nil {
		return nil, err
	}
	file, err := f.root.OpenFile(name, flags|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, executionBoundaryError("files.go open", err)
	}
	if err := regular(file); err != nil {
		_ = f.closeFile(file)
		return nil, err
	}
	return file, nil
}
func (f *Files) Read(name string, out any) error { return f.ReadBounded(name, out, MaxRecord) }

// ReadBounded reads a record published with PublishBounded at the same limit.
func (f *Files) ReadBounded(name string, out any, limit int) error {
	file, err := f.open(name, os.O_RDONLY)
	if err != nil {
		return err
	}
	bytes, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err := errors.Join(readErr, f.closeFile(file)); err != nil {
		return err
	}
	if len(bytes) > limit {
		return fmt.Errorf("execution: record exceeds size bound")
	}
	decoder := json.NewDecoder(strings.NewReader(string(bytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("execution: damaged record: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("execution: trailing record data")
	}
	return nil
}
func (f *Files) syncDirs(name string) error {
	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		if err := f.syncDir(dir); err != nil {
			return err
		}
		if dir == "." {
			return nil
		}
	}
}
func (f *Files) syncDir(dir string) error {
	if err := f.step("directory_sync"); err != nil {
		return err
	}
	file, err := f.root.Open(dir)
	if err != nil {
		return executionBoundaryError("files.go syncDirs", err)
	}
	return errors.Join(durable.Sync(file), file.Close())
}

// Confirm makes an existing record and its namespace durable. Only the file's
// own fsync is skipped, and only for a JSON record this process already synced
// and has not seen change; every directory and device barrier still runs.
func (f *Files) Confirm(name string) error { return f.ConfirmAll(name) }

// stampOf identifies the current bytes of a JSON record. Records are written
// once to a fresh file and replaced by rename, never rewritten in place, so
// an unchanged stamp means the bytes already synced. Locks, anchors and
// capture segments are not records and never get a stamp. Fault injection
// bypasses the memory so tests that script sync steps still see every one.
func (f *Files) stampOf(name string, file *os.File) (fileStamp, bool) {
	if (f.fault != nil && !f.faultKeepsMemory) || !strings.HasSuffix(name, recordExtension) {
		return fileStamp{}, false
	}
	info, err := file.Stat()
	if err != nil {
		return fileStamp{}, false
	}
	return stampFromInfo(info)
}
func (f *Files) wasConfirmed(name string, stamp fileStamp) bool {
	f.confirmMu.Lock()
	defer f.confirmMu.Unlock()
	return f.confirmed[name] == stamp
}

// remember keeps the memory bounded by starting over when it fills; the cost
// of a forgotten record is one more fsync.
func (f *Files) remember(name string, stamp fileStamp) {
	f.confirmMu.Lock()
	defer f.confirmMu.Unlock()
	if _, known := f.confirmed[name]; !known && len(f.confirmed) >= maxConfirmed {
		clear(f.confirmed)
	}
	f.confirmed[name] = stamp
}

// forget drops a removed path, and with tree every path beneath it.
func (f *Files) forget(name string, tree bool) {
	f.confirmMu.Lock()
	defer f.confirmMu.Unlock()
	delete(f.confirmed, name)
	if !tree {
		return
	}
	prefix := name + string(filepath.Separator)
	for path := range f.confirmed {
		if strings.HasPrefix(path, prefix) {
			delete(f.confirmed, path)
		}
	}
}
func (f *Files) Publish(name string, value any, exclusive bool) (Stage, error) {
	return f.PublishBounded(name, value, exclusive, MaxRecord)
}
func (f *Files) PublishBounded(name string, value any, exclusive bool, limit int) (Stage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return BeforePublish, fmt.Errorf("execution: encode bounded record: %w", err)
	}
	if len(data) > limit || limit > MaxBoundedRecord || limit < 1 {
		return BeforePublish, fmt.Errorf("execution: record of %d bytes exceeds its %d byte bound", len(data), limit)
	}
	return f.publishBytes(name, data, exclusive)
}
func (f *Files) publishBytes(name string, data []byte, exclusive bool) (Stage, error) {
	r := f.newRound()
	err := r.stage(name, data, exclusive)
	if err == nil {
		err = r.syncData()
	}
	if err != nil {
		return BeforePublish, errors.Join(err, r.close())
	}
	_, stage, err := r.complete()
	if err = errors.Join(err, r.close()); err != nil {
		if stage == Durable {
			stage = Unconfirmed
		}
		return stage, &PersistenceError{Stage: stage, Err: err}
	}
	return Durable, nil
}
func (f *Files) publishName(temp, name string, exclusive bool) (Stage, error) {
	if !exclusive {
		if err := f.root.Rename(temp, name); err != nil {
			return BeforePublish, executionBoundaryError("publish record name", err)
		}
		return Unconfirmed, nil
	}
	// Result readers can run before Finish returns, so publication must retain one link.
	dir, err := f.root.Open(filepath.Dir(name))
	if err != nil {
		return BeforePublish, executionBoundaryError("open publication directory", err)
	}
	err = renameExclusive(dir, filepath.Base(temp), filepath.Base(name))
	closeErr := dir.Close()
	if err != nil {
		return BeforePublish, executionBoundaryError("exclusively publish record", errors.Join(err, closeErr))
	}
	if closeErr != nil {
		return Unconfirmed, executionBoundaryError("close publication directory", closeErr)
	}
	if err := f.step("publication_visible"); err != nil {
		return Unconfirmed, err
	}
	return Unconfirmed, nil
}

// Ensure creates dir and every missing parent, each with its anchor, and makes
// the whole chain durable in one round.
func (f *Files) Ensure(dir string) error { return f.EnsureWith(dir) }
func (f *Files) Names(dir string) ([]os.DirEntry, error) {
	if err := f.check(dir, false); err != nil {
		return nil, err
	}
	file, err := f.root.Open(dir)
	if err != nil {
		return nil, executionBoundaryError("files.go Names", err)
	}
	entries, readErr := file.ReadDir(-1)
	return entries, errors.Join(readErr, f.closeFile(file))
}
func (f *Files) Remove(name, anchor string) error {
	file, err := f.open(anchor, os.O_RDONLY)
	if err != nil {
		return err
	}
	err = f.check(name, true)
	f.forget(name, false)
	if err == nil {
		err = f.root.Remove(name)
	}
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = f.syncDirs(name)
	}
	if err == nil {
		err = f.flushDevice(file)
	}
	return errors.Join(err, f.closeFile(file))
}

func (f *Files) step(operation string) error {
	if f.observe == nil && f.fault == nil {
		return nil
	}
	// A round syncs files side by side, so test hooks see one step at a time.
	f.hookMu.Lock()
	defer f.hookMu.Unlock()
	if f.observe != nil {
		f.observe(operation)
	}
	if f.fault != nil {
		return f.fault(operation)
	}
	return nil
}
func (f *Files) dataSync(file *os.File) error {
	if err := f.step("data_sync"); err != nil {
		return err
	}
	return executionBoundaryError("files.go dataSync", durable.Sync(file))
}

// flushDevice ends a round: the data and directory syncs before it only
// handed their changes to the device, and this barrier makes them all durable
// at once (one drive cache flush on Darwin, nothing more on Linux).
func (f *Files) flushDevice(file *os.File) error {
	if err := f.step("device_sync"); err != nil {
		return err
	}
	return executionBoundaryError("flush execution namespace device", durable.Barrier(file))
}
func (f *Files) closeFile(file *os.File) error { return errors.Join(file.Close(), f.step("close")) }

// RemoveTree is used only after exact instance validation and a held lifetime fence.
func (f *Files) RemoveTree(name, anchor string) error {
	file, err := f.open(anchor, os.O_RDONLY)
	if err != nil {
		return err
	}
	err = f.check(name, true)
	f.forget(name, true)
	if err == nil {
		err = f.root.RemoveAll(name)
	}
	if err == nil {
		err = f.syncDirs(name)
	}
	if err == nil {
		err = f.flushDevice(file)
	}
	return errors.Join(err, f.closeFile(file))
}

func executionBoundaryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func unlinkedRecord(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return fmt.Errorf("execution: linked private record")
	}
	return nil
}
