package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/shaul/mesh/internal/release"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 || args[0] != "metadata" {
		return errors.New("usage: mesh-release-sign metadata FILE")
	}
	contents, err := readBounded(args[1], release.MaxMetadataBytes)
	if err != nil {
		return err
	}
	_, err = release.DecodeUpdateMetadata(contents)
	if err != nil {
		return fmt.Errorf("validate metadata: %w", err)
	}
	return nil
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // The publisher explicitly selects local input files.
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // The read result is authoritative.
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return contents, nil
}
