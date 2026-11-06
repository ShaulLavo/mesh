package cli

import (
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/spf13/pflag"
)

func TestAgentJSONValueFlagsMatchCobra(t *testing.T) {
	for _, operation := range agentapi.Operations {
		if !operation.Implemented {
			continue
		}
		checkAgentJSONValueFlags(t, operation)
	}
}

func checkAgentJSONValueFlags(t *testing.T, operation agentapi.Operation) {
	t.Helper()
	path := strings.Fields(strings.TrimPrefix(operation.Command, "mesh "))
	command, _, err := NewCommand(Dependencies{}).Find(path)
	if err != nil {
		t.Fatal(err)
	}
	flags := command.Flags()
	flags.AddFlagSet(command.InheritedFlags())
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.NoOptDefVal != "" {
			return
		}
		args := append(append([]string{}, path...), "--json", "--"+flag.Name, "--json=false")
		if !agentapi.Requested(args) || agentapi.Classify(args) != operation.Name {
			t.Errorf("string operand changes early JSON routing: %v", args)
		}
	})
}
