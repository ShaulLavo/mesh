package serve

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// ResolveRoot resolves requestPath to an existing entry beneath root. It
// decodes repeated URL escaping before checking traversal and resolves every
// symlink before verifying confinement. Callers may pass an HTTP or SFTP path.
func ResolveRoot(root, requestPath string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: root is empty", ErrRootUnavailable)
	}
	decoded, err := decodeRequestPath(requestPath)
	if err != nil {
		return "", err
	}
	relative, err := confinedRelativePath(decoded)
	if err != nil {
		return "", err
	}

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve root %s: %w", ErrRootUnavailable, root, err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", fmt.Errorf("%w: resolve root %s: %w", ErrRootUnavailable, root, err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return "", fmt.Errorf("%w: inspect root %s: %w", ErrRootUnavailable, root, err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrRootUnavailable, root)
	}

	candidate := filepath.Join(rootPath, relative)
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("serve: resolve path %q: %w", requestPath, err)
	}
	inside, err := filepath.Rel(rootPath, candidate)
	if err != nil {
		return "", fmt.Errorf("%w: compare %q with root: %v", ErrOutsideRoot, requestPath, err)
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, requestPath)
	}
	return candidate, nil
}

func decodeRequestPath(value string) (string, error) {
	decoded := value
	for range maxPathDecodings {
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
		decoded = next
	}
	return "", fmt.Errorf("%w: path has too many URL-encoding layers", ErrInvalidPath)
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
