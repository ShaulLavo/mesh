// Package reference renders the shipped agent operation registry for the skill bundle.
package reference

import (
	"fmt"
	"github.com/shaul/mesh/internal/agentapi"
	"strings"
)

func Render() string {
	var text strings.Builder
	text.WriteString("# Shipped commands\n\nGenerated from the implemented operation registry. Regenerate with `go generate ./internal/skills`.\n\n")
	for _, operation := range agentapi.Operations {
		if !operation.Implemented {
			continue
		}
		fmt.Fprintf(&text, "## %s\n\n%s\n\n```sh\n%s\n```\n\n", operation.Command, operation.Summary, operation.Example)
		for _, flag := range operation.Flags {
			fmt.Fprintf(&text, "- `%s %s`: %s.\n", flag.Name, flag.Value, flag.Summary)
		}
		text.WriteString("\n")
	}
	return text.String()
}
