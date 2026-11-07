package main

import (
	"fmt"
	"github.com/shaul/mesh/internal/skills/internal/reference"
	"os"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate() error {
	root, err := os.OpenRoot("mesh/references")
	if err != nil {
		return fmt.Errorf("open reference directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("commands.md", []byte(reference.Render()), 0644); err != nil {
		return fmt.Errorf("write command reference: %w", err)
	}
	return nil
}
