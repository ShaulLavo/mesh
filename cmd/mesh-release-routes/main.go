// mesh-release-routes plans forward edges over explicitly supplied local release
// evidence. It neither installs Mesh nor treats those local files as signatures.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

type planningInput struct {
	From   release.Build        `json:"from"`
	At     release.Position     `json:"at"`
	Epoch  uint32               `json:"epoch"`
	Failed []release.FailedEdge `json:"failed,omitempty"`
}

type pageInput struct {
	Epoch       uint32                      `json:"epoch"`
	Page        uint64                      `json:"page"`
	Entries     []release.AnchorEntry       `json:"entries"`
	Previous    map[uint64]release.AssetRef `json:"previous"`
	PublishedBy string                      `json:"publishedBy"`
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("mesh-release-routes", flag.ContinueOnError)
	operation := flags.String("operation", "plan", "plan, page, root, seeds, select, audit, or publish")
	input := flags.String("input", "", "explicit local JSON input")
	documents := flags.String("documents", "", "local evidence directory containing TAG/ASSET files")
	cursor := flags.String("cursor", "", "local pass cursor to save (plan only)")
	destination := flags.String("output-dir", "", "generated publication assets (publish only)")
	local := flags.Bool("local-evidence", false, "acknowledge local evidence is not a verified signature or installation permission")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("routes: arguments: %w", err)
	}
	if *input == "" || flags.NArg() != 0 {
		return errors.New("routes: supply -input JSON and no positional arguments")
	}
	data, err := readLocal(*input, 16<<20)
	if err != nil {
		return err
	}
	switch *operation {
	case "seeds", "select", "audit", "publish":
		if *documents == "" {
			return errors.New("routes: publication needs -documents")
		}
		return publishOperation(*operation, data, *documents, *destination, output)
	case "page":
		return producePage(data, output)
	case "root":
		return produceRoot(data, output)
	case "plan":
		if !*local || *documents == "" || *cursor == "" {
			return errors.New("routes: plan requires -local-evidence, -documents, and -cursor; local input grants no installation permission")
		}
		return plan(ctx, data, *documents, *cursor, output)
	default:
		return errors.New("routes: unknown operation")
	}
}

func plan(ctx context.Context, data []byte, directory, cursorPath string, output io.Writer) error {
	var input planningInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("routes: decode source: %w", err)
	}
	cursor, err := update.LoadDiscoveryCursor(cursorPath)
	if err != nil {
		return fmt.Errorf("routes: load cursor: %w", err)
	}
	cursor.Route.Failed = input.Failed
	source := release.DocumentSource{Read: func(_ context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		return readLocal(filepath.Join(directory, ref.Tag, ref.Name), release.DocumentLimit(kind))
	}, VerifyMetadata: func(context.Context, release.AssetRef, []byte) error { return nil }}
	source = update.CacheDiscoveryDocuments(cursorPath+".documents", source)
	step, err := update.DiscoverNextHop(ctx, input.From, input.At, input.Epoch, cursor, source, release.NewPassBudget())
	if saveErr := update.SaveDiscoveryCursor(cursorPath, step.Cursor); saveErr != nil {
		return fmt.Errorf("routes: save cursor: %w", saveErr)
	}
	if err != nil {
		return fmt.Errorf("routes: plan supplied local evidence: %w", err)
	}
	if err := json.NewEncoder(output).Encode(step); err != nil {
		return fmt.Errorf("routes: write plan: %w", err)
	}
	return nil
}

func producePage(data []byte, output io.Writer) error {
	var input pageInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("routes: decode page input: %w", err)
	}
	page, err := release.BuildAnchorPage(input.Epoch, input.Page, input.Entries, input.Previous, input.PublishedBy)
	if err != nil {
		return fmt.Errorf("routes: build page: %w", err)
	}
	encoded, err := release.EncodeAnchorPage(page, input.PublishedBy)
	if err != nil {
		return fmt.Errorf("routes: encode page: %w", err)
	}
	if _, err := output.Write(encoded); err != nil {
		return fmt.Errorf("routes: write page: %w", err)
	}
	return nil
}

func produceRoot(data []byte, output io.Writer) error {
	var root release.DiscoveryRoot
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("routes: decode root input: %w", err)
	}
	encoded, err := release.EncodeDiscoveryRoot(root)
	if err != nil {
		return fmt.Errorf("routes: encode root: %w", err)
	}
	if _, err := output.Write(encoded); err != nil {
		return fmt.Errorf("routes: write root: %w", err)
	}
	return nil
}

func readLocal(path string, limit int) ([]byte, error) {
	file, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("routes: open local evidence: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("routes: read local evidence: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("routes: local evidence exceeds its byte bound")
	}
	return data, nil
}
