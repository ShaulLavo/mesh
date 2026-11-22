package refdoc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
)

func TestRegistryLinksToAgentOperationsAndReferenceSync(t *testing.T) {
	defs := mcpnative.DefaultRegistry().Definitions()
	if len(defs) == 0 {
		t.Fatal("empty tool registry")
	}
	ops := make(map[string]agentapi.Operation, len(agentapi.Operations))
	for _, op := range agentapi.Operations {
		ops[op.Name] = op
	}
	var names []string
	for _, def := range defs {
		names = append(names, def.Name)
		if def.Operation != "" {
			op, ok := ops[def.Operation]
			if !ok || !op.Implemented {
				t.Errorf("tool %s links to missing or unimplemented operation %q", def.Name, def.Operation)
			}
		}
		var schema map[string]any
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("tool %s has invalid inputSchema: %s", def.Name, def.InputSchema)
		}
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("tool %s has empty description", def.Name)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("tools not sorted by name: %v", names)
	}
	rendered := Render()
	for _, rel := range []string{
		filepath.Join("..", "..", "..", "..", "docs", "reference", "mcp-tools.md"),
		filepath.Join("..", "..", "..", "skills", "mesh", "references", "mcp-tools.md"),
	} {
		got, err := os.ReadFile(rel) //nolint:gosec // fixed repo reference path
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != rendered {
			t.Fatalf("%s drifted from registry; run go generate ./internal/mcpnative", rel)
		}
	}
}
