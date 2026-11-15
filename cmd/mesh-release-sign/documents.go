package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/releasedocuments"
)

func readDocumentInput(path string, destination any) error {
	data, err := readBounded(path, 8<<20)
	if err != nil {
		return signingError(err)
	}
	return signingError(releasedocuments.DecodeJSON(data, destination))
}

func packDocuments(args []string) error {
	if len(args) != 4 {
		return errors.New("usage: mesh-release-sign pack-documents DIRECTORY MEMBERS_JSON BINDING_JSON OUTPUT")
	}
	var members []string
	var binding releasedocuments.Binding
	if err := readDocumentInput(args[1], &members); err != nil {
		return signingError(err)
	}
	if err := readDocumentInput(args[2], &binding); err != nil {
		return signingError(err)
	}
	output, err := os.OpenFile(args[3], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // Explicit publisher destination, exclusive creation.
	if err != nil {
		return signingError(err)
	}
	complete := false
	defer func() {
		_ = output.Close()
		if !complete {
			_ = os.Remove(args[3]) //nolint:gosec // Remove only the explicit exclusively created publisher output.
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := releasedocuments.Pack(ctx, args[0], members, binding, output)
	if err != nil {
		return signingError(err)
	}
	if err := output.Close(); err != nil {
		return signingError(err)
	}
	complete = true
	return signingError(json.NewEncoder(os.Stdout).Encode(result))
}

func materializeDocuments(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: mesh-release-sign materialize-documents INTAKE BINDING_JSON DESTINATION")
	}
	var binding releasedocuments.Binding
	if err := readDocumentInput(args[1], &binding); err != nil {
		return signingError(err)
	}
	return runMaterialization(func(ctx context.Context, trust release.TrustRoot) (releasedocuments.Inventory, error) {
		return releasedocuments.Materialize(ctx, args[0], binding, trust, args[2])
	})
}

func runMaterialization(operation func(context.Context, release.TrustRoot) (releasedocuments.Inventory, error)) error {
	if os.Getenv("MESH_RELEASE_TRUST_CACHE") == "" {
		return errors.New("materialize: explicit authenticated trust cache required")
	}
	trust, err := release.LoadTrustRoot(os.Getenv("MESH_RELEASE_TRUST_CACHE"))
	if err != nil {
		return signingError(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := operation(ctx, trust)
	if err != nil {
		return signingError(err)
	}
	return signingError(json.NewEncoder(os.Stdout).Encode(result))
}

func toolIdentity(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: mesh-release-sign identity")
	}
	build := release.Current()
	if len(build.Commit) != 40 {
		return fmt.Errorf("identity: executable lacks source metadata: %q", build.Commit)
	}
	return signingError(json.NewEncoder(os.Stdout).Encode(struct {
		Schema   int              `json:"schema"`
		Source   string           `json:"source"`
		Target   release.Platform `json:"target"`
		SHA256   string           `json:"sha256"`
		Modified bool             `json:"modified"`
	}{1, build.Commit, build.Platform, build.Digest, build.Modified}))
}

func materializeHistory(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: mesh-release-sign materialize-history CAPTURE EXPECTED_IDENTITY_JSON DESTINATION")
	}
	var expected releasedocuments.HistoricalIdentity
	if err := readDocumentInput(args[1], &expected); err != nil {
		return signingError(err)
	}
	return runMaterialization(func(ctx context.Context, trust release.TrustRoot) (releasedocuments.Inventory, error) {
		return releasedocuments.MaterializeHistory(ctx, args[0], expected, trust, args[2])
	})
}

func signingError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("release tool: %w", err)
}
