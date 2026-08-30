package serve

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxPathDecodings = 8

var (
	// ErrInvalidPath reports a request path that cannot name a safe root entry.
	ErrInvalidPath = errors.New("serve: invalid path")
	// ErrOutsideRoot reports a path whose symlinks resolve outside the root.
	ErrOutsideRoot = errors.New("serve: path escapes root")
	// ErrRootUnavailable reports a missing root or a root that is not a directory.
	ErrRootUnavailable = errors.New("serve: root unavailable")
)

// ResolveRoot resolves requestPath to the canonical name of an existing entry
// beneath root. It is suitable for previews and Realpath-style results. A
// caller that accesses the entry must use OpenRootEntry instead of reopening
// this returned name.
func ResolveRoot(root, requestPath string) (string, error) {
	resolved, err := resolveRootPath(root, requestPath)
	if err != nil {
		return "", err
	}
	return resolved.path, nil
}

type resolvedRootPath struct {
	root     string
	path     string
	relative string
}

func resolveRootPath(root, requestPath string) (resolvedRootPath, error) {
	relative, err := requestRelativePath(requestPath)
	if err != nil {
		return resolvedRootPath{}, err
	}
	rootPath, _, err := canonicalRootPath(root)
	if err != nil {
		return resolvedRootPath{}, err
	}
	return resolveWithinRoot(rootPath, relative, requestPath)
}

func requestRelativePath(requestPath string) (string, error) {
	decoded, err := decodeRequestPath(requestPath)
	if err != nil {
		return "", err
	}
	return confinedRelativePath(decoded)
}

func canonicalRootPath(root string) (string, fs.FileInfo, error) {
	if root == "" {
		return "", nil, fmt.Errorf("%w: root is empty", ErrRootUnavailable)
	}
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return "", nil, fmt.Errorf("%w: resolve root %s: %w", ErrRootUnavailable, root, err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: resolve root %s: %w", ErrRootUnavailable, root, err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: inspect root %s: %w", ErrRootUnavailable, root, err)
	}
	if !rootInfo.IsDir() {
		return "", nil, fmt.Errorf("%w: %s is not a directory", ErrRootUnavailable, root)
	}
	return rootPath, rootInfo, nil
}

func resolveWithinRoot(rootPath, relative, requestPath string) (resolvedRootPath, error) {
	candidate := filepath.Join(rootPath, relative)
	candidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return resolvedRootPath{}, fmt.Errorf("serve: resolve path %q: %w", requestPath, err)
	}
	inside, err := filepath.Rel(rootPath, candidate)
	if err != nil {
		return resolvedRootPath{}, fmt.Errorf("%w: compare %q with root: %v", ErrOutsideRoot, requestPath, err)
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return resolvedRootPath{}, fmt.Errorf("%w: %q", ErrOutsideRoot, requestPath)
	}
	return resolvedRootPath{root: rootPath, path: candidate, relative: inside}, nil
}

// OpenRootEntry resolves and opens one entry through an anchored os.Root. The
// returned metadata describes the returned file descriptor. The caller must
// close the file.
func OpenRootEntry(root, requestPath string) (*os.File, fs.FileInfo, error) {
	relative, err := requestRelativePath(requestPath)
	if err != nil {
		return nil, nil, err
	}
	rootHandle, rootPath, err := openAnchoredRoot(root)
	if err != nil {
		return nil, nil, err
	}
	resolved, resolveErr := resolveWithinRoot(rootPath, relative, requestPath)
	if resolveErr != nil {
		_ = rootHandle.Close()
		return nil, nil, resolveErr
	}
	file, info, openErr := openRootedPath(rootHandle, resolved.relative, resolved.path)
	closeErr := rootHandle.Close()
	if openErr != nil {
		return nil, nil, fmt.Errorf("serve: open rooted path %q: %w", resolved.path, errors.Join(openErr, closeErr))
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("serve: close root %s: %w", resolved.root, closeErr)
	}
	return file, info, nil
}

func openAnchoredRoot(root string) (*os.Root, string, error) {
	if root == "" {
		return nil, "", fmt.Errorf("%w: root is empty", ErrRootUnavailable)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("%w: open root %s: %w", ErrRootUnavailable, root, err)
	}
	handleInfo, err := rootHandle.Stat(".")
	if err != nil {
		_ = rootHandle.Close()
		return nil, "", fmt.Errorf("%w: inspect opened root %s: %v", ErrRootUnavailable, root, err)
	}
	rootPath, rootInfo, err := canonicalRootPath(root)
	if err != nil {
		_ = rootHandle.Close()
		return nil, "", err
	}
	if !os.SameFile(handleInfo, rootInfo) {
		_ = rootHandle.Close()
		return nil, "", fmt.Errorf("%w: root %s changed while opening", ErrRootUnavailable, root)
	}
	return rootHandle, rootPath, nil
}

func openRootedPath(root *os.Root, relative, display string) (*os.File, fs.FileInfo, error) {
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("serve: inspect rooted path %q: %w", display, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("serve: rooted path %q is not a regular file or directory", display)
	}
	return file, info, nil
}

func decodeRequestPath(value string) (string, error) {
	decoded := value
	for decodings := 0; ; decodings++ {
		if strings.IndexByte(decoded, 0) >= 0 {
			return "", fmt.Errorf("%w: path contains a null byte", ErrInvalidPath)
		}
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return "", fmt.Errorf("%w: malformed URL escape: %v", ErrInvalidPath, err)
		}
		if next == decoded {
			return decoded, nil
		}
		if decodings >= maxPathDecodings {
			return "", fmt.Errorf("%w: path has too many URL-encoding layers", ErrInvalidPath)
		}
		decoded = next
	}
}

func confinedRelativePath(decoded string) (string, error) {
	if strings.Contains(decoded, "\\") {
		return "", fmt.Errorf("%w: path contains a backslash", ErrInvalidPath)
	}
	decoded = strings.TrimLeft(decoded, "/")
	segments := strings.Split(decoded, "/")
	clean := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: path contains parent traversal", ErrOutsideRoot)
		default:
			if strings.IndexByte(segment, 0) >= 0 {
				return "", fmt.Errorf("%w: path contains a null byte", ErrInvalidPath)
			}
			clean = append(clean, segment)
		}
	}
	if len(clean) == 0 {
		return ".", nil
	}
	relative := filepath.FromSlash(strings.Join(clean, "/"))
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("%w: path is absolute", ErrOutsideRoot)
	}
	return relative, nil
}
