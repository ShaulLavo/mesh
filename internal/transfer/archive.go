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
	abs, err := filepath.Abs(source)
	if err != nil {
		return Info{}, fmt.Errorf("transfer source path: %w", err)
	}
	stat, err := os.Lstat(abs)
	if err != nil {
		return Info{}, fmt.Errorf("transfer source %s: %w", source, err)
	}
	if stat.Mode()&os.ModeSymlink != 0 {
		return Info{}, fmt.Errorf("transfer source %s is a symbolic link", source)
	}
	if !stat.IsDir() && !stat.Mode().IsRegular() {
		return Info{}, fmt.Errorf("transfer source %s is not a regular file or directory", source)
	}
	name := filepath.Base(abs)
	if !validPeerName(name) {
		return Info{}, fmt.Errorf("transfer source name %q is unsafe", name)
	}

	hash := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(out, hash), transferBufferSize)
	gzipWriter := gzip.NewWriter(buffered)
	tarWriter := tar.NewWriter(gzipWriter)
	closeArchive := func() error {
		return errors.Join(tarWriter.Close(), gzipWriter.Close(), buffered.Flush())
	}
	if stat.IsDir() {
		root, err := os.OpenRoot(abs)
		if err != nil {
			return Info{}, fmt.Errorf("transfer open source %s: %w", source, err)
		}
		openedRoot, statErr := root.Stat(".")
		if statErr != nil || !openedRoot.IsDir() || !os.SameFile(stat, openedRoot) {
			_ = root.Close()
			_ = closeArchive()
			return Info{}, fmt.Errorf("transfer source %s changed while opening", source)
		}
		walkErr := packDirectory(ctx, root, stat.Mode().Perm(), tarWriter)
		closeErr := closeArchive()
		rootErr := root.Close()
		if err := errors.Join(walkErr, closeErr, rootErr); err != nil {
			return Info{}, err
		}
	} else {
		if err := checkContext(ctx); err != nil {
			_ = closeArchive()
			return Info{}, err
		}
		if err := writeRegular(ctx, tarWriter, ".", stat); err != nil {
			_ = closeArchive()
			return Info{}, err
		}
		file, err := os.Open(abs)
		if err != nil {
			_ = closeArchive()
			return Info{}, fmt.Errorf("transfer open source %s: %w", source, err)
		}
		openedStat, statErr := file.Stat()
		if statErr != nil || !openedStat.Mode().IsRegular() || !os.SameFile(stat, openedStat) || openedStat.Size() != stat.Size() {
			_ = file.Close()
			_ = closeArchive()
			return Info{}, fmt.Errorf("transfer source %s changed while opening", source)
		}
		copyErr := copyWithContext(ctx, tarWriter, file, stat.Size())
		closeErr := file.Close()
		archiveErr := closeArchive()
		if err := errors.Join(copyErr, closeErr, archiveErr); err != nil {
			return Info{}, err
		}
	}
	return Info{Name: name, Directory: stat.IsDir(), Digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

func packDirectory(ctx context.Context, root *os.Root, mode fs.FileMode, writer *tar.Writer) error {
	if err := writeDirectory(ctx, writer, ".", mode); err != nil {
		return err
	}
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := checkContext(ctx); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("transfer source entry %s is a symbolic link", name)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("transfer stat source entry %s: %w", name, err)
		}
		name = filepath.ToSlash(name)
		if info.IsDir() {
			return writeDirectory(ctx, writer, name, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("transfer source entry %s is not a regular file or directory", name)
		}
		if err := writeRegular(ctx, writer, name, info); err != nil {
			return err
		}
		file, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("transfer open source entry %s: %w", name, err)
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
			_ = file.Close()
			return fmt.Errorf("transfer source entry %s changed while opening", name)
		}
		copyErr := copyWithContext(ctx, writer, file, info.Size())
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
}

func writeDirectory(ctx context.Context, writer *tar.Writer, name string, mode fs.FileMode) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	return writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Typeflag: tar.TypeDir, Mode: int64(mode.Perm())})
}

func writeRegular(ctx context.Context, writer *tar.Writer, name string, info fs.FileInfo) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	return writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Typeflag: tar.TypeReg, Mode: int64(info.Mode().Perm()), Size: info.Size()})
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, size int64) error {
	reader := contextReader{ctx: ctx, reader: io.LimitReader(src, size)}
	written, err := io.Copy(dst, reader)
	if err != nil {
		return err
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
	stage, err := os.MkdirTemp(parent, ".mesh-copy-stage-*")
	if err != nil {
		return fmt.Errorf("transfer create staging directory: %w", err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	rootMode, err := extractArchive(ctx, archive, info, stage)
	if err != nil {
		return err
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if info.Directory {
		return publishDirectory(ctx, stage, target, destinationExists, info.Name, rootMode, &keepStage)
	}
	return publishFile(ctx, stage, target, destinationExists, &keepStage)
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
	parent := filepath.Dir(abs)
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", false, fmt.Errorf("transfer destination parent %s: %w", parent, err)
	}
	parentInfo, err := os.Stat(canonicalParent)
	if err != nil || !parentInfo.IsDir() {
		return "", "", false, fmt.Errorf("transfer destination parent %s is not a directory", canonicalParent)
	}
	info, err := os.Lstat(filepath.Join(canonicalParent, filepath.Base(abs)))
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", false, fmt.Errorf("transfer destination %s is a symbolic link", destination)
		}
		if info.IsDir() {
			target := filepath.Join(canonicalParent, filepath.Base(abs), name)
			targetInfo, targetErr := os.Lstat(target)
			if targetErr == nil {
				if targetInfo.Mode()&os.ModeSymlink != 0 {
					return "", "", false, fmt.Errorf("transfer destination target %s is a symbolic link", target)
				}
				if targetInfo.IsDir() {
				} else if !targetInfo.Mode().IsRegular() {
					return "", "", false, fmt.Errorf("transfer destination target %s is not a regular file or directory", target)
				}
				return canonicalParent, target, true, nil
			}
			if !errors.Is(targetErr, fs.ErrNotExist) {
				return "", "", false, fmt.Errorf("transfer inspect destination target %s: %w", target, targetErr)
			}
			return canonicalParent, target, false, nil
		}
		if !info.Mode().IsRegular() {
			return "", "", false, fmt.Errorf("transfer destination %s is not a regular file or directory", destination)
		}
		return canonicalParent, filepath.Join(canonicalParent, filepath.Base(abs)), true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", "", false, fmt.Errorf("transfer inspect destination %s: %w", destination, err)
	}
	return canonicalParent, filepath.Join(canonicalParent, filepath.Base(abs)), false, nil
}

func extractArchive(ctx context.Context, archive *os.File, info Info, stage string) (fs.FileMode, error) {
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return 0, fmt.Errorf("transfer open gzip archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	root, err := os.OpenRoot(stage)
	if err != nil {
		return 0, fmt.Errorf("transfer open staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	reader := tar.NewReader(gzipReader)
	seen := make(map[string]bool)
	directoryModes := make(map[string]fs.FileMode)
	rootSeen := false
	var rootMode fs.FileMode
	for {
		if err := checkContext(ctx); err != nil {
			return 0, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if !rootSeen {
				return 0, errors.New("transfer archive has no root entry")
			}
			if info.Directory {
				if err := finalizeDirectoryModes(root, directoryModes); err != nil {
					return 0, err
				}
			}
			return rootMode, nil
		}
		if err != nil {
			return 0, fmt.Errorf("transfer read archive entry: %w", err)
		}
		name := filepath.ToSlash(header.Name)
		if err := validateArchiveName(name); err != nil {
			return 0, err
		}
		if seen[name] {
			return 0, fmt.Errorf("transfer archive repeats entry %q", name)
		}
		seen[name] = true
		if name == "." {
			if rootSeen {
				return 0, errors.New("transfer archive repeats root entry")
			}
			rootSeen = true
			if info.Directory {
				if header.Typeflag != tar.TypeDir {
					return 0, errors.New("transfer directory archive has a non-directory root")
				}
				rootMode = safeMode(fs.FileMode(header.Mode))
				continue
			}
			if !isRegularHeader(header) {
				return 0, errors.New("transfer file archive has a non-regular root")
			}
			if err := extractRegular(ctx, root, "payload", reader, header); err != nil {
				return 0, err
			}
			rootMode = safeMode(fs.FileMode(header.Mode))
			continue
		}
		if !info.Directory {
			return 0, errors.New("transfer file archive contains extra entries")
		}
		if header.Typeflag == tar.TypeDir {
			mode := safeMode(fs.FileMode(header.Mode))
			if err := makeDirectory(root, name, mode); err != nil {
				return 0, err
			}
			directoryModes[name] = mode
			continue
		}
		if !isRegularHeader(header) {
			return 0, fmt.Errorf("transfer archive entry %q is not a regular file or directory", name)
		}
		if err := extractRegular(ctx, root, name, reader, header); err != nil {
			return 0, err
		}
	}
}

func makeDirectory(root *os.Root, name string, mode fs.FileMode) error {
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
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeMode(fs.FileMode(header.Mode)))
	if err != nil {
		return fmt.Errorf("transfer create archive file %q: %w", name, err)
	}
	copyErr := copyWithContext(ctx, file, reader, header.Size)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("transfer extract archive file %q: %w", name, err)
	}
	if err := root.Chmod(name, safeMode(fs.FileMode(header.Mode))); err != nil {
		return fmt.Errorf("transfer set archive file mode %q: %w", name, err)
	}
	return nil
}

func publishDirectory(ctx context.Context, stage, target string, destinationExists bool, name string, mode fs.FileMode, keepStage *bool) error {
	if !destinationExists {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if err := os.Rename(stage, target); err != nil {
			return fmt.Errorf("transfer publish directory %s: %w", target, err)
		}
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("transfer set directory target mode %s: %w", target, err)
		}
		*keepStage = true
		return nil
	}
	info, err := os.Lstat(target)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("transfer destination entry %s conflicts with directory %s", target, name)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("transfer inspect directory target %s: %w", target, err)
	} else if err := os.Mkdir(target, 0700); err != nil {
		return fmt.Errorf("transfer create directory target %s: %w", target, err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return fmt.Errorf("transfer open directory target %s: %w", target, err)
	}
	defer func() { _ = root.Close() }()
	if err := validateMerge(ctx, root, stage, "."); err != nil {
		return err
	}
	if err := mergeTree(ctx, root, stage, "."); err != nil {
		return err
	}
	if err := root.Chmod(".", mode); err != nil {
		return fmt.Errorf("transfer set directory target mode %s: %w", target, err)
	}
	return nil
}

func publishFile(ctx context.Context, stage, target string, destinationExists bool, keepStage *bool) error {
	if destinationExists {
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("transfer inspect file target %s: %w", target, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || !info.Mode().IsRegular() {
			return fmt.Errorf("transfer file target %s is not a regular file", target)
		}
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".mesh-copy-file-*")
	if err != nil {
		return fmt.Errorf("transfer create file target temporary: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	source, err := os.Open(filepath.Join(stage, "payload"))
	if err != nil {
		_ = temp.Close()
		return fmt.Errorf("transfer open staged file: %w", err)
	}
	copyErr := copyWithContext(ctx, temp, source, fileSize(source))
	closeSourceErr := source.Close()
	closeTempErr := temp.Close()
	if err := errors.Join(copyErr, closeSourceErr, closeTempErr); err != nil {
		return fmt.Errorf("transfer write file target temporary: %w", err)
	}
	mode, err := os.Stat(filepath.Join(stage, "payload"))
	if err != nil {
		return fmt.Errorf("transfer stat staged file: %w", err)
	}
	if err := os.Chmod(tempName, safeMode(mode.Mode())); err != nil {
		return fmt.Errorf("transfer set file target mode: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
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
		name := entry.Name()
		rel := name
		if relative != "." {
			rel = path.Join(relative, name)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("transfer staging entry %s is a symbolic link", rel)
		}
		info, err := root.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			if entry.IsDir() {
				if err := validateMerge(ctx, root, stage, rel); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("transfer inspect merge target %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("transfer merge target %s is a symbolic link", rel)
		}
		if entry.IsDir() != info.IsDir() {
			return fmt.Errorf("transfer merge target %s conflicts with source", rel)
		}
		if entry.IsDir() {
			if err := validateMerge(ctx, root, stage, rel); err != nil {
				return err
			}
		}
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
		name := entry.Name()
		rel := name
		if relative != "." {
			rel = path.Join(relative, name)
		}
		if entry.IsDir() {
			entryInfo, infoErr := entry.Info()
			if infoErr != nil {
				return fmt.Errorf("transfer stat staged directory %s: %w", rel, infoErr)
			}
			if err := root.Mkdir(rel, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("transfer create merge directory %s: %w", rel, err)
			}
			if err := mergeTree(ctx, root, stage, rel); err != nil {
				return err
			}
			if err := root.Chmod(rel, safeMode(entryInfo.Mode())); err != nil {
				return fmt.Errorf("transfer set merge directory mode %s: %w", rel, err)
			}
			continue
		}
		if err := mergeFile(ctx, root, stage, rel, entry); err != nil {
			return err
		}
	}
	return nil
}

func mergeFile(ctx context.Context, root *os.Root, stage, relative string, entry os.DirEntry) error {
	entryInfo, err := entry.Info()
	if err != nil {
		return fmt.Errorf("transfer stat staged merge file %s: %w", relative, err)
	}
	source, err := os.Open(filepath.Join(stage, relative))
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

func rejectSymlinks(root string) error {
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("transfer destination contains symbolic link %s", name)
		}
		return nil
	})
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
	return header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
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
	return ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := checkContext(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
