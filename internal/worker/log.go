package worker

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/shaul/mesh/internal/paths"
)

// ReadLogTail reads at most limit bytes from the end of a durable worker log.
func ReadLogTail(sessionDir string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("worker: log tail must be positive")
	}
	path := paths.Log(sessionDir)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("worker: inspect log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("worker: log %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("worker: open log %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read result takes precedence

	size := info.Size()
	start := int64(0)
	if size > int64(limit) {
		start = size - int64(limit)
	}
	output := make([]byte, size-start)
	n, err := file.ReadAt(output, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("worker: read log %s: %w", path, err)
	}
	return output[:n], nil
}
