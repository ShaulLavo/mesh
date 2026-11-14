package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

// This fixture calls the production helper preparer. It starts no service and
// supplies no alternate adoption, signature, or installation implementation.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) != 5 {
		return errors.New("usage: native-helper STATE EXECUTABLE SERVICE-DIRECTORY KIND DOMAIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	installed, err := updateinstall.PrepareHelper(ctx, updateinstall.HelperConfig{
		StateDir: arguments[0], Executable: arguments[1], ServiceDir: arguments[2], Kind: arguments[3], Domain: arguments[4],
	})
	if err != nil {
		return fmt.Errorf("prepare actual historical helper: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(installed); err != nil {
		return fmt.Errorf("write helper fixture receipt: %w", err)
	}
	return nil
}
