package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

func verifyContextManifest(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("boundary: open context manifest directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, err := readProofFile(root, filepath.Base(path))
	if err != nil {
		return err
	}
	if len(data) > 512<<10 {
		return errors.New("boundary: context manifest exceeds 512 KiB")
	}
	var manifest release.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("boundary: decode context manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("boundary: invalid rollback context manifest: %w", err)
	}
	return nil
}
