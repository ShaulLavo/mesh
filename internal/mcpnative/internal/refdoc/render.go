// Package refdoc renders the Markdown native MCP tool reference from the registry.
package refdoc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shaul/mesh/internal/mcpnative"
)

// Render generates docs/reference/mcp-tools.md from DefaultRegistry.
func Render() string {
	defs := mcpnative.DefaultRegistry().Definitions()
	var b strings.Builder
	b.WriteString("# Native MCP tools\n\n")
	b.WriteString("Generated from the native MCP tool registry (`internal/mcpnative`). Regenerate with `go generate ./internal/mcpnative`.\n\n")
	b.WriteString("| Tool | Linked operation | Mode | Summary |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, def := range defs {
		op := def.Operation
		if op == "" {
			op = "mcp-native"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", def.Name, op, annotationSummary(def.Annotations), def.Title)
	}
	b.WriteString("\n")
	for _, def := range defs {
		renderToolSection(&b, def)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderToolSection(b *strings.Builder, def mcpnative.ToolDef) {
	fmt.Fprintf(b, "## %s\n\n", def.Name)
	b.WriteString(def.Description + "\n\n")
	if def.Operation != "" {
		fmt.Fprintf(b, "- Linked operation: `%s`\n", def.Operation)
	}
	fmt.Fprintf(b, "- Hints: %s\n", annotationSummary(def.Annotations))
	renderSchemaFields(b, def.InputSchema)
	b.WriteString("\n")
}

func renderSchemaFields(b *strings.Builder, raw json.RawMessage) {
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) == 0 {
		b.WriteString("- Arguments: none\n")
		return
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString("- Arguments:\n")
	for _, name := range names {
		prop := schema.Properties[name]
		req := "optional"
		if required[name] {
			req = "required"
		}
		extra := ""
		if len(prop.Enum) > 0 {
			extra = " (" + strings.Join(prop.Enum, ", ") + ")"
		}
		fmt.Fprintf(b, "  - `%s` (`%s`, %s)%s: %s\n", name, prop.Type, req, extra, prop.Description)
	}
}

func annotationSummary(a mcpnative.ToolAnnotations) string {
	parts := make([]string, 0, 4)
	if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
		parts = append(parts, "read-only")
	}
	if a.DestructiveHint != nil && *a.DestructiveHint {
		parts = append(parts, "destructive")
	}
	if a.IdempotentHint != nil && *a.IdempotentHint {
		parts = append(parts, "idempotent")
	}
	if a.OpenWorldHint != nil && *a.OpenWorldHint {
		parts = append(parts, "open-world")
	}
	if len(parts) == 0 {
		return "mutating"
	}
	return strings.Join(parts, ", ")
}
