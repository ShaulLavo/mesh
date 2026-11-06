package agentapi

import "strings"

type Flag struct {
	Name, Value string
	Required    bool
	Summary     string
}
type Operation struct {
	Name, Command, Args           string
	Flags                         []Flag
	Mutates, Network, Implemented bool
	Summary, Example              string
}

var Operations = []Operation{
	{Name: "services.list", Command: "mesh serve ls", Flags: []Flag{{Name: "--json", Required: true, Summary: "write one agent result"}, {Name: "--timeout", Value: "D", Summary: "hard deadline for live host queries"}}, Network: true, Implemented: true, Summary: "List live and cached services without opening their URLs.", Example: "mesh serve ls --json"},
	{Name: "hosts.list", Command: "mesh hosts"},
	{Name: "hosts.inspect", Command: "mesh inspect", Args: "HOST"},
	{Name: "sessions.list", Command: "mesh session list"},
	{Name: "sessions.inspect", Command: "mesh session inspect", Args: "SESSION_ID"},
	{Name: "profile.show", Command: "mesh profile show"},
	{Name: "profile.set", Command: "mesh profile set"},
	{Name: "profile.edit", Command: "mesh profile edit"},
	{Name: "profile.status", Command: "mesh profile status"},
	{Name: "skill.show", Command: "mesh skill show"},
	{Name: "skill.install", Command: "mesh skill install"},
	{Name: "skill.update", Command: "mesh skill update"},
	{Name: "skill.status", Command: "mesh skill status"},
	{Name: "skill.uninstall", Command: "mesh skill uninstall"},
	{Name: "sessions.create", Command: "mesh session create"},
	{Name: "sessions.request", Command: "mesh session request"},
	{Name: "sessions.wait", Command: "mesh session wait"},
	{Name: "sessions.logs", Command: "mesh session logs"},
	{Name: "sessions.signal", Command: "mesh session signal"},
	{Name: "sessions.kill", Command: "mesh session kill"},
	{Name: "sessions.remove", Command: "mesh session rm"},
}

var Legacy = []string{"mesh version --json", "mesh update --json", "mesh app --json", "mesh device identity --json", "mesh update-helper --json", "mesh wake arm --json"}

// Classify recognizes implemented entry points before Cobra and deployment initialization.
func Classify(args []string) string {
	words := invocationWords(args)
	if len(words) >= 2 && words[0] == "serve" && words[1] == "list" {
		words[1] = "ls"
	}
	for _, operation := range Operations {
		path := strings.Fields(strings.TrimPrefix(operation.Command, "mesh "))
		if operation.Implemented && matchesPath(words, path) {
			return operation.Name
		}
	}
	return ""
}

func matchesPath(words, path []string) bool {
	if len(words) < len(path) {
		return false
	}
	for i, word := range path {
		if words[i] != word {
			return false
		}
	}
	return true
}

func invocationWords(args []string) []string {
	words := make([]string, 0, 2)
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if value {
			value = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			value = valueFlag(arg)
			continue
		}
		words = append(words, arg)
	}
	return words
}

func valueFlag(arg string) bool {
	switch arg {
	case "--leave-key", "--host", "--timeout":
		return true
	}
	return false
}
