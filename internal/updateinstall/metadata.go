package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Open special files without waiting for a peer, then inspect and read the same
// descriptor. Walk beneath the selected directory without following symlinks.
func readMetadata(ctx context.Context, directory, name string, private bool) ([]byte, error) {
	return readMetadataChecked(ctx.Err, directory, name, private)
}

func metadataCancellation(check func() error) error {
	if check == nil {
		return nil
	}
	return check()
}

func readMetadataChecked(check func() error, directory, name string, private bool) ([]byte, error) {
	if err := metadataCancellation(check); err != nil {
		return nil, fmt.Errorf("metadata read cancelled: %w", err)
	}
	file, err := openMetadata(directory, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(file) }()
	var stat unix.Stat_t
	if err = unix.Fstat(file, &stat); err != nil {
		return nil, fmt.Errorf("inspect installation metadata: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("installation metadata requires a regular file")
	}
	permissions := uint32(0022)
	if private {
		permissions = 0077
	}
	if uint32(stat.Mode)&permissions != 0 {
		return nil, errors.New("installation metadata has unsafe permissions")
	}
	if stat.Size > journalLimit {
		return nil, errors.New("installation metadata exceeds size limit")
	}
	return readMetadataDescriptor(check, file)
}

func openMetadata(directory, name string) (int, error) {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name {
		return -1, errors.New("installation metadata requires a local path")
	}
	file, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open installation metadata directory: %w", err)
	}
	parts := strings.Split(name, string(os.PathSeparator))
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(file, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(file)
		if openErr != nil {
			return -1, fmt.Errorf("open installation metadata subdirectory: %w", openErr)
		}
		file = next
	}
	defer func() { _ = unix.Close(file) }()
	result, err := unix.Openat(file, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open installation metadata: %w", err)
	}
	return result, nil
}

func readMetadataDescriptor(check func() error, file int) ([]byte, error) {
	var data []byte
	buffer := make([]byte, 32<<10)
	for {
		if err := metadataCancellation(check); err != nil {
			return nil, fmt.Errorf("metadata read cancelled: %w", err)
		}
		count, err := unix.Read(file, buffer)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read installation metadata: %w", err)
		}
		if count == 0 {
			return data, nil
		}
		if len(data)+count > journalLimit {
			return nil, errors.New("installation metadata exceeds size limit")
		}
		data = append(data, buffer[:count]...)
	}
}
