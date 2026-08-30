package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const (
	// MaximumPreviewEntries bounds one public directory safety scan.
	MaximumPreviewEntries = 100_000
	maximumPreviewDepth   = 128
	directoryReadBatch    = 256
)

var (
	// ErrCredentialsFound reports that a public directory contains a name that
	// commonly holds credentials. The caller must require an explicit override.
	ErrCredentialsFound = errors.New("serve: credential-like entry found")
	// ErrDirectoryLimit reports that a public-directory safety scan exceeded a
	// fixed resource bound.
	ErrDirectoryLimit = errors.New("serve: directory scan limit exceeded")
)

// Preview is the origin-authoritative interpretation of one requested service.
// FileCount is populated for public directory services after a complete bounded
// safety scan.
type Preview struct {
	Service   Service
	FileCount uint64
}

type directoryScan struct {
	fileCount     uint64
	credentialRel string
}

type scanDirectory struct {
	path  string
	depth int
}

// InspectService resolves a service exactly as the origin daemon will store it.
// Relative directory targets are rooted at the daemon user's home directory.
// Public directories are scanned on every call; allowCredentials bypasses only
// credential-like names, never traversal, I/O, or resource-limit failures.
func InspectService(ctx context.Context, home string, service Service, allowCredentials bool) (Preview, error) {
	if ctx == nil {
		return Preview{}, errors.New("serve: nil inspection context")
	}
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if service.PublicName == "" {
		if service.WakeOnRequest {
			return Preview{}, errors.New("serve: wake-on-request requires a public service")
		}
		if allowCredentials {
			return Preview{}, errors.New("serve: credential override requires a public service")
		}
	}
	if len(service.Target) > MaximumServiceTargetBytes {
		return Preview{}, fmt.Errorf("serve: service target exceeds %d bytes", MaximumServiceTargetBytes)
	}

	numericTarget := isNumericTarget(service.Target)
	if service.Kind == "" {
		if numericTarget {
			service.Kind = Proxy
		} else {
			service.Kind = Static
		}
	} else if (service.Kind == Static || service.Kind == Files) && numericTarget {
		return Preview{}, errors.New("serve: a numeric target is a proxy port, not a directory")
	}
	if allowCredentials && service.Kind == Proxy {
		return Preview{}, errors.New("serve: credential override applies only to public directories")
	}

	if service.Kind == Static || service.Kind == Files {
		resolved, err := resolveDirectoryTarget(home, service.Target)
		if err != nil {
			return Preview{}, err
		}
		service.Target = resolved
	}
	normalized, err := normalizeService(service)
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{Service: normalized}
	if normalized.PublicName == "" || normalized.Kind == Proxy {
		return preview, nil
	}

	scan, err := scanPublicDirectory(ctx, normalized.Target, MaximumPreviewEntries, maximumPreviewDepth)
	if err != nil {
		return Preview{}, err
	}
	if scan.credentialRel != "" && !allowCredentials {
		return Preview{}, fmt.Errorf("%w: %q", ErrCredentialsFound, scan.credentialRel)
	}
	preview.FileCount = scan.fileCount
	return preview, nil
}

func resolveDirectoryTarget(home, target string) (string, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return "", errors.New("serve: daemon home must be a clean absolute path")
	}
	if target == "" {
		return "", errors.New("serve: directory target is empty")
	}
	if strings.IndexByte(target, 0) >= 0 {
		return "", errors.New("serve: directory target contains a null byte")
	}
	candidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(home, candidate)
	}
	resolved, err := ResolveRoot(candidate, "/")
	if err != nil {
		return "", fmt.Errorf("serve: resolve directory target: %w", err)
	}
	return resolved, nil
}

func isNumericTarget(target string) bool {
	if target == "" {
		return false
	}
	for _, character := range target {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func scanPublicDirectory(ctx context.Context, root string, maxEntries, maxDepth int) (directoryScan, error) {
	if ctx == nil {
		return directoryScan{}, errors.New("serve: nil directory scan context")
	}
	if err := ctx.Err(); err != nil {
		return directoryScan{}, err
	}
	if maxEntries <= 0 || maxDepth < 0 {
		return directoryScan{}, ErrDirectoryLimit
	}
	rootHandle, canonicalRoot, err := openAnchoredRoot(root)
	if err != nil {
		return directoryScan{}, err
	}
	defer rootHandle.Close() //nolint:errcheck // scan errors take precedence over cleanup

	result := directoryScan{}
	if credential := credentialAncestor(canonicalRoot); credential != "" {
		result.credentialRel = credential
	}
	visitedDirectories := map[string]struct{}{canonicalRoot: {}}
	visitedFiles := make(map[string]struct{})
	stack := []scanDirectory{{path: canonicalRoot}}
	entries := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return directoryScan{}, err
		}
		last := len(stack) - 1
		directory := stack[last]
		stack = stack[:last]
		if directory.depth > maxDepth {
			return directoryScan{}, fmt.Errorf("%w: directory depth exceeds %d", ErrDirectoryLimit, maxDepth)
		}
		directoryRelative, err := filepath.Rel(canonicalRoot, directory.path)
		if err != nil {
			return directoryScan{}, fmt.Errorf("%w: compare public directory: %v", ErrOutsideRoot, err)
		}
		file, directoryInfo, err := openRootedPath(rootHandle, directoryRelative, directory.path)
		if err != nil {
			return directoryScan{}, fmt.Errorf("serve: open public directory: %w", err)
		}
		if !directoryInfo.IsDir() {
			_ = file.Close()
			return directoryScan{}, fmt.Errorf("serve: public entry %q is not a directory", relativeDisplayPath(canonicalRoot, directory.path))
		}
		for {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return directoryScan{}, err
			}
			batch, readErr := file.ReadDir(directoryReadBatch)
			for _, entry := range batch {
				entries++
				if entries > maxEntries {
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("%w: more than %d entries", ErrDirectoryLimit, maxEntries)
				}
				candidate := filepath.Join(directory.path, entry.Name())
				resolved, resolveErr := filepath.EvalSymlinks(candidate)
				if resolveErr != nil {
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("serve: resolve public entry %q: %w", relativeDisplayPath(canonicalRoot, candidate), resolveErr)
				}
				if !withinRoot(canonicalRoot, resolved) {
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("%w: %q", ErrOutsideRoot, relativeDisplayPath(canonicalRoot, candidate))
				}
				resolvedRelative, relativeErr := filepath.Rel(canonicalRoot, resolved)
				if relativeErr != nil {
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("%w: compare public entry %q: %v", ErrOutsideRoot, relativeDisplayPath(canonicalRoot, candidate), relativeErr)
				}
				entryFile, info, openErr := openRootedPath(rootHandle, resolvedRelative, resolved)
				if openErr != nil {
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("serve: open public entry %q: %w", relativeDisplayPath(canonicalRoot, candidate), openErr)
				}
				switch {
				case info.IsDir():
					if closeErr := entryFile.Close(); closeErr != nil {
						_ = file.Close()
						return directoryScan{}, fmt.Errorf("serve: close public entry %q: %w", relativeDisplayPath(canonicalRoot, candidate), closeErr)
					}
					if credentialLike(entry.Name()) && result.credentialRel == "" {
						result.credentialRel = relativeDisplayPath(canonicalRoot, candidate)
					}
					if _, seen := visitedDirectories[resolved]; !seen {
						visitedDirectories[resolved] = struct{}{}
						stack = append(stack, scanDirectory{path: resolved, depth: directory.depth + 1})
					}
				case info.Mode().IsRegular():
					if closeErr := entryFile.Close(); closeErr != nil {
						_ = file.Close()
						return directoryScan{}, fmt.Errorf("serve: close public entry %q: %w", relativeDisplayPath(canonicalRoot, candidate), closeErr)
					}
					if _, seen := visitedFiles[resolved]; !seen {
						visitedFiles[resolved] = struct{}{}
						result.fileCount++
					}
					if credentialLike(entry.Name()) && result.credentialRel == "" {
						result.credentialRel = relativeDisplayPath(canonicalRoot, candidate)
					}
				default:
					_ = entryFile.Close()
					_ = file.Close()
					return directoryScan{}, fmt.Errorf("serve: public entry %q is not a regular file or directory", relativeDisplayPath(canonicalRoot, candidate))
				}
			}
			switch readErr {
			case nil:
				continue
			case io.EOF:
				if closeErr := file.Close(); closeErr != nil {
					return directoryScan{}, fmt.Errorf("serve: close public directory: %w", closeErr)
				}
			default:
				_ = file.Close()
				return directoryScan{}, fmt.Errorf("serve: read public directory: %w", readErr)
			}
			break
		}
	}
	return result, nil
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func relativeDisplayPath(root, candidate string) string {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." {
		return filepath.Base(candidate)
	}
	return filepath.ToSlash(relative)
}

func credentialLike(name string) bool {
	lower := strings.ToLower(name)
	return lower == ".env" || strings.HasPrefix(lower, ".env.") ||
		lower == ".git" || lower == ".ssh" ||
		strings.HasPrefix(lower, "id_") || strings.HasSuffix(lower, ".pem")
}

func credentialAncestor(root string) string {
	for current := filepath.Clean(root); ; current = filepath.Dir(current) {
		base := filepath.Base(current)
		if credentialLike(base) {
			return base
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
	}
}
