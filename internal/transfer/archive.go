package transfer

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const transferBufferSize = 64 * 1024

// Info describes the source represented by an archive.
type Info struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Digest    string `json:"digest"`
}

// Temporary returns an unnamed file in the destination's storage directory.
// The file is unlinked immediately, so an interrupted transfer cannot leave a
// named archive behind.
func Temporary(destination string) (*os.File, error) {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return nil, fmt.Errorf("transfer temporary path: %w", err)
	}
	parent := filepath.Dir(abs)
	info, statErr := os.Lstat(abs)
	if statErr == nil && info.IsDir() {
		parent = abs
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("transfer temporary destination %s: %w", destination, statErr)
	}
	file, err := os.CreateTemp(parent, ".mesh-copy-*")
	if err != nil {
		return nil, fmt.Errorf("transfer temporary file in %s: %w", parent, err)
	}
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("transfer unlink temporary file: %w", err)
	}
	return file, nil
}

// Pack streams source as a gzip-compressed tar archive and returns metadata
// needed to place it at a destination. Source links and special files are
// refused so an archive has only the two portable entry kinds copy supports.
func Pack(ctx context.Context, source string, out io.Writer) (Info, error) {
	abs, stat, err := inspectSource(source)
	if err != nil {
		return Info{}, err
	}
	hash := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(out, hash), transferBufferSize)
	compressed := gzip.NewWriter(buffered)
	writer := tar.NewWriter(compressed)
	packErr := packSource(ctx, abs, stat, writer)
	closeErr := errors.Join(writer.Close(), compressed.Close(), buffered.Flush())
	if err := errors.Join(packErr, closeErr); err != nil {
		return Info{}, err
	}
	return Info{Name: filepath.Base(abs), Directory: stat.IsDir(), Digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

func inspectSource(source string) (string, fs.FileInfo, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return "", nil, fmt.Errorf("transfer source path: %w", err)
	}
	stat, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("transfer source %s: %w", source, err)
	}
	if !stat.IsDir() && !stat.Mode().IsRegular() {
		return "", nil, fmt.Errorf("transfer source %s is not a regular file or directory", source)
	}
	if !validPeerName(filepath.Base(abs)) {
		return "", nil, fmt.Errorf("transfer source name %q is unsafe", filepath.Base(abs))
	}
	return abs, stat, nil
}

func packSource(ctx context.Context, source string, stat fs.FileInfo, writer *tar.Writer) error {
	if !stat.IsDir() {
		return packSourceFile(ctx, source, stat, writer)
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return fmt.Errorf("transfer open source directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("transfer stat source directory: %w", err)
	}
	if !opened.IsDir() || !os.SameFile(stat, opened) {
		return errors.New("transfer source directory changed while opening")
	}
	if err := writeDirectory(ctx, writer, ".", stat.Mode().Perm()); err != nil {
		return err
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("transfer walk source %s: %w", name, walkErr)
		}
		if name == "." {
			return nil
		}
		return packDirectoryEntry(ctx, root, writer, name, entry)
	})
	if err != nil {
		return fmt.Errorf("transfer pack source directory: %w", err)
	}
	return nil
}

func packSourceFile(ctx context.Context, source string, stat fs.FileInfo, writer *tar.Writer) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // source is the explicitly selected local path; opened identity is checked before reading.
	if err != nil {
		return fmt.Errorf("transfer open source file: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := checkOpenedFile(file, stat); err != nil {
		return err
	}
	if err := writeRegular(ctx, writer, ".", stat); err != nil {
		return err
	}
	return copyWithContext(ctx, writer, file, stat.Size())
}

func packDirectoryEntry(ctx context.Context, root *os.Root, writer *tar.Writer, name string, entry fs.DirEntry) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	stat, err := entry.Info()
	if err != nil {
		return fmt.Errorf("transfer stat source entry %s: %w", name, err)
	}
	if stat.IsDir() {
		return writeDirectory(ctx, writer, name, stat.Mode().Perm())
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("transfer source entry %s is not a regular file or directory", name)
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("transfer open source entry %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	if err := checkOpenedFile(file, stat); err != nil {
		return err
	}
	if err := writeRegular(ctx, writer, name, stat); err != nil {
		return err
	}
	return copyWithContext(ctx, writer, file, stat.Size())
}

func checkOpenedFile(file *os.File, previous fs.FileInfo) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("transfer stat opened file: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(previous, opened) || opened.Size() != previous.Size() {
		return errors.New("transfer source file changed while opening")
	}
	return nil
}

func writeDirectory(ctx context.Context, writer *tar.Writer, name string, mode fs.FileMode) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Typeflag: tar.TypeDir, Mode: int64(mode.Perm())}); err != nil {
		return fmt.Errorf("transfer write directory header %s: %w", name, err)
	}
	return nil
}

func writeRegular(ctx context.Context, writer *tar.Writer, name string, info fs.FileInfo) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Typeflag: tar.TypeReg, Mode: int64(info.Mode().Perm()), Size: info.Size()}); err != nil {
		return fmt.Errorf("transfer write file header %s: %w", name, err)
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, size int64) error {
	reader := contextReader{ctx: ctx, reader: io.LimitReader(src, size)}
	written, err := io.Copy(dst, reader)
	if err != nil {
		return fmt.Errorf("transfer copy bytes: %w", err)
	}
	if written != size {
		return fmt.Errorf("transfer source changed while reading: copied %d of %d bytes", written, size)
	}
	return checkContext(ctx)
}

// Receive validates archive bytes and extracts them into a staging directory
// before changing the destination. A directory source merges its tree; a file
// replaces its target atomically.
func Receive(ctx context.Context, archive *os.File, info Info, destination string) error {
	if archive == nil {
		return errors.New("transfer archive is nil")
	}
	if !validPeerName(info.Name) {
		return fmt.Errorf("transfer archive name %q is unsafe", info.Name)
	}
	if !validDigest(info.Digest) {
		return errors.New("transfer archive digest is invalid")
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("transfer seek archive: %w", err)
	}
	hash := sha256.New()
	if err := copyWithContext(ctx, hash, archive, archiveSize(archive)); err != nil {
		return fmt.Errorf("transfer hash archive: %w", err)
	}
	actual := hash.Sum(nil)
	expected, _ := hex.DecodeString(info.Digest)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return errors.New("transfer archive digest mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("transfer seek archive for extraction: %w", err)
	}

	parent, target, destinationExists, err := resolveDestination(destination, info.Name)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, fmt.Sprintf(".mesh-copy-stage-%d-*", os.Getpid()))
	if err != nil {
		return fmt.Errorf("transfer create staging directory: %w", err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	modes, err := extractArchive(ctx, archive, info, stage)
	if err != nil {
		return err
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if info.Directory {
		return publishDirectory(ctx, stage, target, destinationExists, info.Name, modes, &keepStage)
	}
	return publishFile(ctx, stage, target, destinationExists, modes["."])
}

func archiveSize(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil || info.Size() < 0 {
		return 0
	}
	return info.Size()
}

func resolveDestination(destination, name string) (string, string, bool, error) {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return "", "", false, fmt.Errorf("transfer destination path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", false, fmt.Errorf("transfer destination parent: %w", err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", "", false, fmt.Errorf("transfer open destination parent: %w", err)
	}
	defer func() { _ = root.Close() }()
	target := filepath.Base(abs)
	stat, err := inspectDestination(root, target)
	if err != nil {
		return "", "", false, err
	}
	if stat == nil {
		return parent, filepath.Join(parent, target), false, nil
	}
	if !stat.IsDir() {
		return parent, filepath.Join(parent, target), true, nil
	}
	target = filepath.Join(target, name)
	stat, err = inspectDestination(root, target)
	if err != nil {
		return "", "", false, err
	}
	return filepath.Dir(filepath.Join(parent, target)), filepath.Join(parent, target), stat != nil, nil
}

func inspectDestination(root *os.Root, name string) (fs.FileInfo, error) {
	stat, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("transfer inspect destination %s: %w", name, err)
	}
	if !stat.IsDir() && !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("transfer destination %s is not a regular file or directory", name)
	}
	return stat, nil
}

type archiveExtractor struct {
	ctx       context.Context
	root      *os.Root
	directory bool
	seen      map[string]bool
	modes     map[string]fs.FileMode
}

func extractArchive(ctx context.Context, archive *os.File, info Info, stage string) (map[string]fs.FileMode, error) {
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("transfer open gzip archive: %w", err)
	}
	defer func() { _ = compressed.Close() }()
	root, err := os.OpenRoot(stage)
	if err != nil {
		return nil, fmt.Errorf("transfer open staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	extractor := archiveExtractor{ctx: ctx, root: root, directory: info.Directory, seen: make(map[string]bool), modes: make(map[string]fs.FileMode)}
	reader := tar.NewReader(compressed)
	for {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("transfer read archive entry: %w", err)
		}
		if err := extractor.entry(reader, header); err != nil {
			return nil, err
		}
	}
	if !extractor.seen["."] {
		return nil, errors.New("transfer archive has no root entry")
	}
	if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, reader: compressed}); err != nil {
		return nil, fmt.Errorf("transfer validate gzip footer: %w", err)
	}
	return extractor.modes, nil
}

func (e *archiveExtractor) entry(reader io.Reader, header *tar.Header) error {
	name := header.Name
	if err := validateArchiveName(name); err != nil {
		return err
	}
	if e.seen[name] {
		return fmt.Errorf("transfer archive repeats entry %q", name)
	}
	e.seen[name] = true
	e.modes[name] = fs.FileMode(header.Mode & 0777)
	if name == "." {
		return e.rootEntry(reader, header)
	}
	if !e.directory {
		return errors.New("transfer file archive contains extra entries")
	}
	if header.Typeflag == tar.TypeDir {
		return makeDirectory(e.root, name)
	}
	if !isRegularHeader(header) {
		return fmt.Errorf("transfer archive entry %q is not a regular file or directory", name)
	}
	return extractRegular(e.ctx, e.root, name, reader, header)
}

func (e *archiveExtractor) rootEntry(reader io.Reader, header *tar.Header) error {
	if e.directory {
		if header.Typeflag != tar.TypeDir {
			return errors.New("transfer directory archive has a non-directory root")
		}
		return nil
	}
	if !isRegularHeader(header) {
		return errors.New("transfer file archive has a non-regular root")
	}
	return extractRegular(e.ctx, e.root, "payload", reader, header)
}

func makeDirectory(root *os.Root, name string) error {
	if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
		return fmt.Errorf("transfer create archive parent for %q: %w", name, err)
	}
	if err := root.Mkdir(name, 0700); err != nil {
		info, statErr := root.Lstat(name)
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("transfer create archive directory %q: %w", name, err)
		}
	}
	return nil
}

func finalizeDirectoryModes(root *os.Root, modes map[string]fs.FileMode) error {
	ordered := make([]string, 0, len(modes))
	for name := range modes {
		ordered = append(ordered, name)
	}
	slices.SortFunc(ordered, func(left, right string) int {
		if left == right {
			return 0
		}
		if left == "." {
			return 1
		}
		if right == "." {
			return -1
		}
		return strings.Count(right, "/") - strings.Count(left, "/")
	})
	for _, name := range ordered {
		if err := root.Chmod(name, modes[name]); err != nil {
			return fmt.Errorf("transfer set archive directory mode %q: %w", name, err)
		}
	}
	return nil
}

func extractRegular(ctx context.Context, root *os.Root, name string, reader io.Reader, header *tar.Header) error {
	if header.Size < 0 {
		return fmt.Errorf("transfer archive entry %q has a negative size", name)
	}
	if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
		return fmt.Errorf("transfer create archive parent for %q: %w", name, err)
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("transfer create archive file %q: %w", name, err)
	}
	copyErr := copyWithContext(ctx, file, reader, header.Size)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("transfer extract archive file %q: %w", name, err)
	}
	return nil
}

func publishDirectory(ctx context.Context, stage, target string, destinationExists bool, name string, modes map[string]fs.FileMode, keepStage *bool) error {
	if !destinationExists {
		return publishNewDirectory(ctx, stage, target, modes, keepStage)
	}
	root, err := openExistingDirectory(target, name)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := validateMerge(ctx, root, stage, "."); err != nil {
		return err
	}
	if err := mergeTree(ctx, root, stage, "."); err != nil {
		return err
	}
	return finalizeDirectoryModes(root, modes)
}

func openExistingDirectory(target, name string) (*os.Root, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("transfer inspect directory target %s: %w", target, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("transfer destination entry %s conflicts with directory %s", target, name)
	}
	parent, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return nil, fmt.Errorf("transfer open directory target parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	root, err := parent.OpenRoot(filepath.Base(target))
	if err != nil {
		return nil, fmt.Errorf("transfer open directory target %s: %w", target, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("transfer destination directory changed while opening")
	}
	return root, nil
}

func publishNewDirectory(ctx context.Context, stage, target string, modes map[string]fs.FileMode, keepStage *bool) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(stage))
	if err != nil {
		return fmt.Errorf("transfer open directory publication parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	relative, err := filepath.Rel(filepath.Dir(stage), target)
	if err != nil {
		return fmt.Errorf("transfer directory publication path: %w", err)
	}
	root, err := parent.OpenRoot(filepath.Base(stage))
	if err != nil {
		return fmt.Errorf("transfer open staged publication directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := parent.Rename(filepath.Base(stage), relative); err != nil {
		return fmt.Errorf("transfer publish directory %s: %w", target, err)
	}
	*keepStage = true
	return finalizeDirectoryModes(root, modes)
}

func publishFile(ctx context.Context, stage, target string, destinationExists bool, mode fs.FileMode) error {
	parent, err := os.OpenRoot(filepath.Dir(stage))
	if err != nil {
		return fmt.Errorf("transfer open file publication parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	relative, err := filepath.Rel(filepath.Dir(stage), target)
	if err != nil {
		return fmt.Errorf("transfer file publication path: %w", err)
	}
	if destinationExists {
		info, err := parent.Lstat(relative)
		if err != nil {
			return fmt.Errorf("transfer inspect file target %s: %w", target, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("transfer file target %s is not a regular file", target)
		}
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	payload := filepath.Join(filepath.Base(stage), "payload")
	if err := parent.Chmod(payload, mode); err != nil {
		return fmt.Errorf("transfer set file target mode: %w", err)
	}
	if err := parent.Rename(payload, relative); err != nil {
		return fmt.Errorf("transfer publish file %s: %w", target, err)
	}
	return nil
}

func validateMerge(ctx context.Context, root *os.Root, stage, relative string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(stage, relative))
	if err != nil {
		return fmt.Errorf("transfer read staged directory %s: %w", relative, err)
	}
	for _, entry := range entries {
		rel := path.Join(relative, entry.Name())
		if err := validateMergeEntry(ctx, root, stage, rel, entry); err != nil {
			return err
		}
	}
	return nil
}

func validateMergeEntry(ctx context.Context, root *os.Root, stage, relative string, entry fs.DirEntry) error {
	info, err := root.Lstat(relative)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("transfer inspect merge target %s: %w", relative, err)
	}
	if info != nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("transfer merge target %s is a symbolic link", relative)
		}
		if entry.IsDir() != info.IsDir() || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("transfer merge target %s conflicts with source", relative)
		}
	}
	if entry.IsDir() {
		return validateMerge(ctx, root, stage, relative)
	}
	return nil
}

func mergeTree(ctx context.Context, root *os.Root, stage, relative string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(stage, relative))
	if err != nil {
		return fmt.Errorf("transfer read staged directory %s: %w", relative, err)
	}
	for _, entry := range entries {
		rel := path.Join(relative, entry.Name())
		if err := mergeEntry(ctx, root, stage, rel, entry); err != nil {
			return err
		}
	}
	return nil
}

func mergeEntry(ctx context.Context, root *os.Root, stage, relative string, entry fs.DirEntry) error {
	if !entry.IsDir() {
		return mergeFile(ctx, root, stage, relative, entry)
	}
	if err := root.Mkdir(relative, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("transfer create merge directory %s: %w", relative, err)
	}
	return mergeTree(ctx, root, stage, relative)
}

func mergeFile(ctx context.Context, root *os.Root, stage, relative string, entry os.DirEntry) error {
	entryInfo, err := entry.Info()
	if err != nil {
		return fmt.Errorf("transfer stat staged merge file %s: %w", relative, err)
	}
	source, err := os.Open(filepath.Join(stage, relative)) //nolint:gosec // stage is receiver-owned and relative names came from the validated archive tree.
	if err != nil {
		return fmt.Errorf("transfer open staged merge file %s: %w", relative, err)
	}
	defer func() { _ = source.Close() }()
	tempName := ".mesh-copy-file-"
	for index := 0; ; index++ {
		candidate := fmt.Sprintf("%s%d", tempName, index)
		file, createErr := root.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeMode(entryInfo.Mode()))
		if errors.Is(createErr, fs.ErrExist) {
			continue
		}
		if createErr != nil {
			return fmt.Errorf("transfer create merge temporary %s: %w", relative, createErr)
		}
		copyErr := copyWithContext(ctx, file, source, fileSize(source))
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			_ = root.Remove(candidate)
			return fmt.Errorf("transfer write merge file %s: %w", relative, err)
		}
		if err := root.Chmod(candidate, safeMode(entryInfo.Mode())); err != nil {
			_ = root.Remove(candidate)
			return fmt.Errorf("transfer set merge file mode %s: %w", relative, err)
		}
		if err := root.Rename(candidate, relative); err != nil {
			_ = root.Remove(candidate)
			return fmt.Errorf("transfer publish merge file %s: %w", relative, err)
		}
		return nil
	}
}

func validateArchiveName(name string) error {
	if name == "." {
		return nil
	}
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." {
		return fmt.Errorf("transfer archive entry %q has an unsafe path", name)
	}
	return nil
}

func validPeerName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00")
}

func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func isRegularHeader(header *tar.Header) bool {
	return header.Typeflag == tar.TypeReg || header.Typeflag == byte(0)
}

func safeMode(mode fs.FileMode) fs.FileMode {
	return fs.FileMode(mode) & 0777
}

func fileSize(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil || info.Size() < 0 {
		return 0
	}
	return info.Size()
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("transfer interrupted: %w", err)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := checkContext(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(p) //nolint:wrapcheck // io.Reader must preserve EOF identity for io.Copy and tar readers.
}
