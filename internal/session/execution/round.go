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
	"slices"
	"strings"
	"sync"
	"syscall"
)

// Record is one JSON record for PublishAll and Prepare.
type Record struct {
	Name      string
	Value     any
	Exclusive bool
}

// A round makes several files durable together: their data is synced side by
// side, each distinct directory on their paths is synced once, deepest first,
// and each device holding them gets one final barrier. Every file is synced
// before any rename and every rename precedes the first directory sync, so a
// round proves for each name what proving them one at a time would.
//
// Only names that do not depend on one another may share a round. A record
// that must never exist without another one goes in a later round, because
// rename order is not a durability order: until the round's barriers finish,
// a crash may keep any subset of its renames. The exception is a file inside
// a directory the same round creates, which is reachable only through that
// directory and so cannot outlive it.
type round struct {
	f       *Files
	entries []roundEntry
}

type roundKind int

const (
	// roundCreated is an anchor or empty file the round opened or created.
	roundCreated roundKind = iota
	// roundExisting is a record the round confirms in place.
	roundExisting
	// roundStaged is a record written to temp and renamed to name.
	roundStaged
)

type roundEntry struct {
	kind      roundKind
	name      string
	temp      string
	exclusive bool
	file      *os.File
	stamp     fileStamp
	stamped   bool
}

func (f *Files) newRound() *round { return &round{f: f} }

// close releases every file and removes temporary names left behind.
func (r *round) close() error {
	var err error
	for _, e := range r.entries {
		err = errors.Join(err, r.f.closeFile(e.file))
		if e.temp != "" {
			_ = r.f.root.Remove(e.temp)
		}
	}
	return err
}

// ensure creates dir and every missing parent, with the regular anchor in each
// that keeps Darwin's device barrier after a namespace sync.
func (r *round) ensure(dir string) error {
	if !validPath(dir) {
		return fmt.Errorf("execution: invalid directory")
	}
	parts := strings.Split(dir, string(filepath.Separator))
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		err := r.f.root.Mkdir(name, 0700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return executionBoundaryError("files.go ensure", err)
		}
		if err := r.f.check(name, false); err != nil {
			return err
		}
		if err := r.open(roundCreated, filepath.Join(name, ".barrier"), os.O_CREATE|os.O_RDWR); err != nil {
			return err
		}
	}
	return nil
}

func (r *round) open(kind roundKind, name string, flags int) error {
	// Directories that share parents share their anchors.
	if slices.ContainsFunc(r.entries, func(e roundEntry) bool { return e.name == name }) {
		return nil
	}
	file, err := r.f.open(name, flags)
	if err != nil {
		return err
	}
	r.entries = append(r.entries, roundEntry{kind: kind, name: name, file: file})
	return nil
}

// stage writes a record's bytes to a temporary name beside it.
func (r *round) stage(name string, data []byte, exclusive bool) error {
	if err := r.f.check(name, true); err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return executionBoundaryError("files.go stage", err)
	}
	temp := filepath.Join(filepath.Dir(name), ".tmp-"+hex.EncodeToString(random[:]))
	file, err := r.f.open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return err
	}
	r.entries = append(r.entries, roundEntry{kind: roundStaged, name: name, temp: temp, exclusive: exclusive, file: file})
	n, err := file.Write(data)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	return executionBoundaryError("files.go stage", err)
}

// syncData syncs every file's own bytes side by side. An existing record this
// process already proved durable and has not seen change skips its fsync; its
// directory and device barriers still run.
func (r *round) syncData() error {
	if len(r.entries) == 1 {
		return r.syncEntry(&r.entries[0])
	}
	errs := make([]error, len(r.entries))
	var group sync.WaitGroup
	for i := range r.entries {
		group.Go(func() { errs[i] = r.syncEntry(&r.entries[i]) })
	}
	group.Wait()
	return errors.Join(errs...)
}

func (r *round) syncEntry(e *roundEntry) error {
	if e.kind == roundExisting {
		e.stamp, e.stamped = r.f.stampOf(e.name, e.file)
		if e.stamped && r.f.wasConfirmed(e.name, e.stamp) {
			return nil
		}
	}
	return r.f.dataSync(e.file)
}

// publish renames the staged records in order. It returns how many became
// visible and, on failure, the stage the failed one reached.
func (r *round) publish() (int, Stage, error) {
	visible := 0
	for i := range r.entries {
		e := &r.entries[i]
		if e.kind != roundStaged {
			continue
		}
		if err := r.f.step("publish"); err != nil {
			return visible, BeforePublish, err
		}
		stage, err := r.f.publishName(e.temp, e.name, e.exclusive)
		if stage != BeforePublish {
			e.temp = ""
			visible++
		}
		if err != nil {
			return visible, stage, err
		}
		// Sampled right after the rename, before the slower directory syncs,
		// so it describes the bytes that were just synced.
		e.stamp, e.stamped = r.f.stampOf(e.name, e.file)
	}
	return visible, Unconfirmed, nil
}

// complete renames the staged records and runs the round's barriers, after
// syncData succeeded. Records it proved durable are remembered.
func (r *round) complete() (int, Stage, error) {
	visible, stage, err := r.publish()
	if err != nil {
		return visible, stage, err
	}
	names := make([]string, len(r.entries))
	files := make([]*os.File, len(r.entries))
	for i, e := range r.entries {
		names[i], files[i] = e.name, e.file
	}
	err = r.f.syncDirsOf(names)
	if err == nil {
		err = r.f.flushDevices(files)
	}
	if err != nil {
		return visible, Unconfirmed, err
	}
	for _, e := range r.entries {
		if e.stamped {
			r.f.remember(e.name, e.stamp)
		}
	}
	return visible, Durable, nil
}

// syncDirsOf syncs every directory from each name's parent up to the state
// root, each distinct one once, deeper directories before their parents.
func (f *Files) syncDirsOf(names []string) error {
	if len(names) == 1 {
		return f.syncDirs(names[0])
	}
	var dirs []string
	for _, name := range names {
		dirs = appendChain(dirs, name)
	}
	slices.SortStableFunc(dirs, func(a, b string) int { return pathDepth(b) - pathDepth(a) })
	for _, dir := range dirs {
		if err := f.syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// appendChain adds every directory from name's parent up to the state root
// that dirs does not hold yet.
func appendChain(dirs []string, name string) []string {
	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
		if dir == "." {
			return dirs
		}
	}
}

func pathDepth(dir string) int {
	if dir == "." {
		return 0
	}
	return strings.Count(dir, string(filepath.Separator)) + 1
}

// flushDevices issues one device barrier per device among files. A file
// whose device cannot be read gets its own barrier.
func (f *Files) flushDevices(files []*os.File) error {
	seen := make(map[uint64]bool, 1)
	for _, file := range files {
		dev, ok := fileDevice(file)
		if ok && seen[dev] {
			continue
		}
		if ok {
			seen[dev] = true
		}
		if err := f.flushDevice(file); err != nil {
			return err
		}
	}
	return nil
}

// fileDevice is deviceOf, a variable so tests can give files distinct or
// unknown devices.
var fileDevice = deviceOf

func deviceOf(file *os.File) (uint64, bool) {
	info, err := file.Stat()
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:gosec,unconvert // Dev is signed on Darwin and unsigned on Linux
}

// ConfirmAll proves several existing records durable in one round. The names
// must not depend on one another's durability order.
func (f *Files) ConfirmAll(names ...string) error {
	r := f.newRound()
	var err error
	for _, name := range names {
		if err = r.open(roundExisting, name, os.O_RDONLY); err != nil {
			break
		}
	}
	if err == nil {
		err = r.syncData()
	}
	if err == nil {
		_, _, err = r.complete()
	}
	return errors.Join(err, r.close())
}

// EnsureWith creates dir with any missing parents, and the named empty files
// in it, and makes all of them durable in one round.
func (f *Files) EnsureWith(dir string, empty ...string) error {
	_, err := f.Prepare(dir, empty)
	return err
}

// PublishAll publishes independent records in one round and returns the names
// that became visible, also on failure.
func (f *Files) PublishAll(records ...Record) ([]string, error) {
	return f.Prepare("", nil, records...)
}

// Prepare creates dir with any missing parents and the named empty files in
// it, publishes records, and makes all of it durable in one round. An empty
// dir creates nothing. It returns the record names that became visible, in
// order, also on failure. Records must not depend on one another; they may be
// inside dir, which the round itself makes durable.
func (f *Files) Prepare(dir string, empty []string, records ...Record) ([]string, error) {
	r := f.newRound()
	visible, err := r.prepare(dir, empty, records)
	return visible, errors.Join(err, r.close())
}

func (r *round) prepare(dir string, empty []string, records []Record) ([]string, error) {
	if err := r.add(dir, empty, records); err != nil {
		return nil, err
	}
	if err := r.syncData(); err != nil {
		return nil, err
	}
	count, stage, err := r.complete()
	visible := make([]string, 0, count)
	for _, e := range r.entries {
		if e.kind == roundStaged && len(visible) < count {
			visible = append(visible, e.name)
		}
	}
	if err != nil && stage != BeforePublish {
		err = &PersistenceError{Stage: stage, Err: err}
	}
	return visible, err
}

func (r *round) add(dir string, empty []string, records []Record) error {
	if dir != "" {
		if err := r.ensure(dir); err != nil {
			return err
		}
	}
	for _, name := range empty {
		if err := r.open(roundCreated, filepath.Join(dir, name), os.O_CREATE|os.O_RDWR); err != nil {
			return err
		}
	}
	for _, record := range records {
		if err := r.record(record.Name, record.Value, record.Exclusive, MaxRecord); err != nil {
			return err
		}
	}
	return nil
}

// record stages a JSON record of at most limit bytes.
func (r *round) record(name string, value any, exclusive bool, limit int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("execution: encode record %s: %w", name, err)
	}
	if len(data) > limit || limit > MaxBoundedRecord || limit < 1 {
		return fmt.Errorf("execution: record of %d bytes exceeds its %d byte bound", len(data), limit)
	}
	return r.stage(name, data, exclusive)
}

// run syncs, publishes and confirms everything added to the round.
func (r *round) run() error {
	if err := r.syncData(); err != nil {
		return err
	}
	_, stage, err := r.complete()
	if err != nil && stage != BeforePublish {
		err = &PersistenceError{Stage: stage, Err: err}
	}
	return err
}
