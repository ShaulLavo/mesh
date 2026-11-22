package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/mcpnative/internal/refdoc"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate() error {
	content := []byte(refdoc.Render())
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return fmt.Errorf("locate repository root: %w", err)
	}
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, rel := range []string{
		filepath.Join("docs", "reference", "mcp-tools.md"),
		filepath.Join("internal", "skills", "mesh", "references", "mcp-tools.md"),
	} {
		if err := root.WriteFile(rel, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}
