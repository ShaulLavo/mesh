package daemon

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// Every file operation reaches its file through directory descriptors. A path
// is walked from the root one component at a time, each opened with
// O_NOFOLLOW, and everything after that (stat, open, enumerate, stage,
// rename, unlink) is relative to the descriptor of the directory that was
// opened. A rename or symlink swap of a parent after the walk cannot redirect
// the operation, and cleanup cannot miss the directory it staged in.

// fileStat is the part of a stat the file controls use.
type fileStat struct {
	dev, ino, size uint64
	mode           uint32
	mtime, ctime   int64
}

func (s fileStat) kind() uint32       { return s.mode & unix.S_IFMT }
func (s fileStat) regular() bool      { return s.kind() == unix.S_IFREG }
func (s fileStat) directory() bool    { return s.kind() == unix.S_IFDIR }
func (s fileStat) symlink() bool      { return s.kind() == unix.S_IFLNK }
func (s fileStat) permission() uint32 { return s.mode & 0o7777 }

// sameFile reports whether two stats name one inode.
func (s fileStat) sameFile(other fileStat) bool { return s.dev == other.dev && s.ino == other.ino }

// revision fingerprints identity and content-change state. Replacing the file
// changes the inode, and writing it in place changes size, modification time
// or change time.
func (s fileStat) revision() string {
	return protocol.FileRevisionString(s.dev, s.ino, s.size, s.mtime, s.ctime)
}

func (s fileStat) describe() string {
	switch s.kind() {
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFLNK:
		return "a symlink"
	case unix.S_IFIFO:
		return "a named pipe"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	default:
		return "a special file"
	}
}

func fstatFile(file *os.File) (fileStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return fileStat{}, fmt.Errorf("daemon: stat %s: %w", file.Name(), err)
	}
	return statFrom(&st), nil
}

// statAt stats name inside dir without following a symlink.
func statAt(dir *os.File, name string) (fileStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileStat{}, fmt.Errorf("daemon: stat %s: %w", name, err)
	}
	return statFrom(&st), nil
}

// openDirectory walks an absolute path from the root without following any
// symlink and returns the directory it names.
func openDirectory(path string) (*os.File, error) {
	current, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileOSFailure("open", "/", err)
	}
	walked := ""
	for _, name := range pathComponents(path) {
		next, err := unix.Openat(current, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			failure := componentFailure(current, walked, name, err)
			_ = unix.Close(current)
			return nil, failure
		}
		_ = unix.Close(current)
		current = next
		walked += "/" + name
	}
	return os.NewFile(uintptr(current), path), nil //nolint:gosec // a file descriptor is never negative here
}

func pathComponents(path string) []string {
	if path == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

// componentFailure explains why one path component could not be entered. A
// symlink anywhere in the path is refused, and the message names its target so
// the caller can ask for the real path.
func componentFailure(parent int, walked, name string, err error) error {
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		return fileOSFailure("open", walked+"/"+name, err)
	}
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && statFrom(&st).symlink() {
		target, _ := os.Readlink(walked + "/" + name)
		return fileFailure(agentapi.FileNotRegular, "daemon: %s/%s is a symlink to %q; name the real path", walked, name, target)
	}
	return fileFailure(agentapi.FileNotRegular, "daemon: %s/%s is not a directory", walked, name)
}

// fileLocation is a file's name inside its opened parent directory.
type fileLocation struct {
	dir  *os.File
	name string
	path string
}

func (l fileLocation) close() { _ = l.dir.Close() }

// locateFile opens the parent directory of path. The root has no name and is
// not a file.
func locateFile(path string) (fileLocation, error) {
	if path == "/" {
		return fileLocation{}, fileFailure(agentapi.UsageInvalid, "daemon: the root directory is not a file")
	}
	dir, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return fileLocation{}, err
	}
	return fileLocation{dir: dir, name: filepath.Base(path), path: path}, nil
}

// openRegular opens the located file for reading and refuses anything that is
// not a regular file. The kind is checked before the open, so a named pipe or
// device is never opened, and again on the descriptor, so a swap between the
// two cannot substitute one.
func (l fileLocation) openRegular() (*os.File, fileStat, error) {
	before, err := statAt(l.dir, l.name)
	if err != nil {
		return nil, fileStat{}, fileOSFailure("stat", l.path, err)
	}
	if !before.regular() {
		return nil, fileStat{}, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s, not a regular file", l.path, before.describe())
	}
	fd, err := unix.Openat(int(l.dir.Fd()), l.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileStat{}, fileOSFailure("open", l.path, err)
	}
	file := os.NewFile(uintptr(fd), l.path) //nolint:gosec // a file descriptor is never negative here
	st, err := fstatFile(file)
	if err != nil || !st.regular() {
		_ = file.Close()
		if err != nil {
			return nil, fileStat{}, fileOSFailure("stat", l.path, err)
		}
		return nil, fileStat{}, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s, not a regular file", l.path, st.describe())
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = file.Close()
		return nil, fileStat{}, fileOSFailure("configure", l.path, err)
	}
	return file, st, nil
}

// openChildDirectory enters a subdirectory of dir without following a link.
func openChildDirectory(dir *os.File, name, path string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileOSFailure("open", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil //nolint:gosec // a file descriptor is never negative here
}

// stagedFile is a new file being prepared next to its destination.
type stagedFile struct {
	dir  *os.File
	name string
	file *os.File
}

// createStaged creates an exclusive, owner-only file in dir for the new
// contents. The caller owns removing it.
func createStaged(dir *os.File, path string) (stagedFile, error) {
	for range 4 {
		var random [8]byte
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return stagedFile{}, fileOSFailure("create temporary file for", path, err)
		}
		name := fmt.Sprintf(".mesh-fs-%x", random)
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return stagedFile{}, fileOSFailure("create temporary file for", path, err)
		}
		return stagedFile{dir: dir, name: name, file: os.NewFile(uintptr(fd), name)}, nil //nolint:gosec // a file descriptor is never negative here
	}
	return stagedFile{}, fileFailure(agentapi.InternalError, "daemon: could not choose a temporary name beside %s", path)
}

// discard removes the staged name through the directory descriptor it was
// created in, wherever the directory's path points now.
func (s stagedFile) discard() {
	_ = s.file.Close()
	_ = unix.Unlinkat(int(s.dir.Fd()), s.name, 0)
}

// fill writes the contents and permission bits and makes them durable.
func (s stagedFile) fill(data []byte, permission uint32) error {
	if _, err := s.file.Write(data); err != nil {
		return fmt.Errorf("daemon: write temporary file %s: %w", s.name, err)
	}
	if err := unix.Fchmod(int(s.file.Fd()), permission); err != nil {
		return fmt.Errorf("daemon: set mode of temporary file %s: %w", s.name, err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("daemon: sync temporary file %s: %w", s.name, err)
	}
	return nil
}

// syncDirectory makes a rename durable. A filesystem that cannot sync a
// directory has already accepted the rename, so a failure here is not one.
func syncDirectory(dir *os.File) { _ = dir.Sync() }

// isNotSupported reports an exchange or no-replace rename the filesystem does
// not implement.
func isNotSupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, fs.ErrInvalid)
}

// wrapRename names the failed rename and keeps the errno for errors.Is.
func wrapRename(err error) error {
	if err != nil {
		return fmt.Errorf("daemon: rename: %w", err)
	}
	return nil
}
