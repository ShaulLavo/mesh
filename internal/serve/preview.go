package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Preview is the origin-authoritative interpretation of one requested service.
type Preview struct {
	Service Service
}

// InspectService resolves a service exactly as the origin daemon will store it.
// Relative directory targets are rooted at the daemon user's home directory.
func InspectService(ctx context.Context, home string, service Service) (Preview, error) {
	if ctx == nil {
		return Preview{}, errors.New("serve: nil inspection context")
	}
	if err := ctx.Err(); err != nil {
		return Preview{}, err
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

	if service.Demand != nil && service.Demand.Cwd != "" && !filepath.IsAbs(service.Demand.Cwd) {
		// A client on another machine cannot know this host's home, so a
		// relative directory is this host's user's, as a directory TARGET is.
		demand := *service.Demand
		demand.Cwd = filepath.Join(home, demand.Cwd)
		service.Demand = &demand
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
	if normalized.Demand != nil {
		info, err := os.Stat(normalized.Demand.Cwd)
		if err != nil {
			return Preview{}, fmt.Errorf("serve: service %q working directory: %w", normalized.Name, err)
		}
		if !info.IsDir() {
			return Preview{}, fmt.Errorf("serve: service %q working directory %s is not a directory", normalized.Name, normalized.Demand.Cwd)
		}
	}
	return Preview{Service: normalized}, nil
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
