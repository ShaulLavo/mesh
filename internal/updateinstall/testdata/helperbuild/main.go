package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 3 && os.Args[1] == "version" && os.Args[2] == "--json" {
		if err := json.NewEncoder(os.Stdout).Encode(release.Current()); err != nil {
			return fmt.Errorf("report build: %w", err)
		}
		return nil
	}
	if len(os.Args) != 5 || os.Args[1] != "update-helper" || os.Args[2] != "--state-dir" || os.Args[4] != "--check-journal" {
		return fmt.Errorf("unsupported fixture command")
	}
	settings, err := updateinstall.ReadSettings(os.Args[3])
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	_, err = updateinstall.New(settings.Config())
	if err != nil {
		return fmt.Errorf("check journal: %w", err)
	}
	return nil
}
