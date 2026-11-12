// Package execution owns durable keyed-command records, below worker and daemon.
package execution

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const MaxRecord = 64 << 10

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
	return &Files{root: root}, nil
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
func (f *Files) Read(name string, out any) error {
	file, err := f.open(name, os.O_RDONLY)
	if err != nil {
		return err
	}
	bytes, readErr := io.ReadAll(io.LimitReader(file, MaxRecord+1))
	if err := errors.Join(readErr, f.closeFile(file)); err != nil {
		return err
	}
	if len(bytes) > MaxRecord {
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
		if err := f.step("directory_sync"); err != nil {
			return err
		}
		file, err := f.root.Open(dir)
		if err != nil {
			return executionBoundaryError("files.go syncDirs", err)
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
		if dir == "." {
			return nil
		}
	}
}
func (f *Files) Confirm(name string) error {
	file, err := f.open(name, os.O_RDONLY)
	if err != nil {
		return err
	}
	err = errors.Join(f.dataSync(file), f.syncDirs(name))
	if err == nil {
		err = f.flushDevice(file)
	}
	return errors.Join(err, f.closeFile(file))
}
func (f *Files) Publish(name string, value any, exclusive bool) (Stage, error) {
	return f.PublishBounded(name, value, exclusive, MaxRecord)
}
func (f *Files) PublishBounded(name string, value any, exclusive bool, limit int) (Stage, error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > limit || limit > 256<<10 || limit < 1 {
		return BeforePublish, fmt.Errorf("execution: encode bounded record: %w", err)
	}
	return f.publishBytes(name, data, exclusive)
}
func (f *Files) publishBytes(name string, data []byte, exclusive bool) (Stage, error) {
	if err := f.check(name, true); err != nil {
		return BeforePublish, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return BeforePublish, executionBoundaryError("files.go Publish", err)
	}
	temp := filepath.Join(filepath.Dir(name), ".tmp-"+hex.EncodeToString(random[:]))
	file, err := f.open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return BeforePublish, err
	}
	defer func() { _ = f.root.Remove(temp) }()
	n, err := file.Write(data)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	if err = errors.Join(err, f.dataSync(file)); err != nil {
		return BeforePublish, errors.Join(err, f.closeFile(file))
	}
	stage := BeforePublish
	err = f.step("publish")
	if err == nil {
		stage, err = f.publishName(temp, name, exclusive)
	}
	if err != nil {
		return stage, &PersistenceError{Stage: stage, Err: errors.Join(err, f.closeFile(file))}
	}
	err = f.syncDirs(name)
	if err == nil {
		err = f.flushDevice(file)
	}
	if err = errors.Join(err, f.closeFile(file)); err != nil {
		return Unconfirmed, &PersistenceError{Unconfirmed, err}
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
func (f *Files) Ensure(dir string) error {
	if !validPath(dir) {
		return fmt.Errorf("execution: invalid directory")
	}
	parts := strings.Split(dir, string(filepath.Separator))
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		if err := f.ensureOne(name); err != nil {
			return err
		}
	}
	return nil
}
func (f *Files) ensureOne(name string) error {
	err := f.root.Mkdir(name, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return executionBoundaryError("files.go ensureOne", err)
	}
	if err := f.check(name, false); err != nil {
		return err
	}
	// A regular anchor keeps Darwin's final device barrier after every namespace sync.
	anchor := filepath.Join(name, ".barrier")
	file, err := f.open(anchor, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	err = errors.Join(f.dataSync(file), f.syncDirs(anchor))
	if err == nil {
		err = f.flushDevice(file)
	}
	return errors.Join(err, f.closeFile(file))
}
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
	if f.fault != nil {
		return f.fault(operation)
	}
	return nil
}
func (f *Files) dataSync(file *os.File) error {
	if err := f.step("data_sync"); err != nil {
		return err
	}
	return executionBoundaryError("files.go dataSync", file.Sync())
}
func (f *Files) flushDevice(file *os.File) error {
	if err := f.step("device_sync"); err != nil {
		return err
	}
	return deviceSync(file)
}
func (f *Files) closeFile(file *os.File) error { return errors.Join(file.Close(), f.step("close")) }

// RemoveTree is used only after exact instance validation and a held lifetime fence.
func (f *Files) RemoveTree(name, anchor string) error {
	file, err := f.open(anchor, os.O_RDONLY)
	if err != nil {
		return err
	}
	err = f.check(name, true)
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
