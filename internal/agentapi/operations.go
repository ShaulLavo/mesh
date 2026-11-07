package agentapi

import "strings"

const keyDirFlag = "--key-dir"
const identityFlag = "--identity"
const expectedRevisionFlag = "--expected-revision"
const pathFlagValue = "PATH"
const jsonFlag = "--json"
const timeoutFlag = "--timeout"
const jsonFlagSummary = "write one agent result"

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
	{Name: "scope.keygen", Command: "mesh device scope keygen", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: keyDirFlag, Value: pathFlagValue, Required: true, Summary: "explicit dedicated credential directory"}}, Summary: "Create a separate metadata credential without enrolling it.", Example: "mesh device scope keygen --key-dir KEY_DIRECTORY --json"},
	{Name: "scope.grant", Command: "mesh device scope grant", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: identityFlag, Value: "ID", Required: true}, {Name: "--permissions", Value: "metadata", Required: true}, {Name: expectedRevisionFlag, Value: "N", Required: true}, {Name: "--expires-at", Value: "TIME"}, {Name: "--expires-in", Value: "D"}}, Summary: "Grant metadata access on the target's local policy.", Example: "mesh device scope grant --identity PUBLIC_ID --permissions metadata --expected-revision 0 --json"},
	{Name: "scope.revoke", Command: "mesh device scope revoke", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: identityFlag, Value: "ID", Required: true}, {Name: expectedRevisionFlag, Value: "N", Required: true}}, Summary: "Remove a scoped key from local target policy.", Example: "mesh device scope revoke --identity PUBLIC_ID --expected-revision 1 --json"},
	{Name: "scope.list", Command: "mesh device scope list", Implemented: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}}, Summary: "Read the local target's scope policy.", Example: "mesh device scope list --json"},
	{Name: "scope.read", Command: "mesh device scope read", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--host", Value: "ID", Required: true}, {Name: keyDirFlag, Value: pathFlagValue, Required: true}, {Name: "--section", Value: "SECTION"}, {Name: "--session", Value: "ID"}, {Name: "--cursor", Value: "TOKEN"}, {Name: "--limit", Value: "N"}, {Name: timeoutFlag, Value: "D"}}, Summary: "Read a narrow metadata page over explicit-key TLS without fallbacks.", Example: "mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY --json"},
	{Name: "services.list", Command: "mesh serve ls", Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for live host queries"}}, Network: true, Implemented: true, Summary: "List live and cached services without opening their URLs.", Example: "mesh serve ls --json"},
	{Name: "hosts.list", Command: "mesh hosts", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read machine context without waking machines.", Example: "mesh hosts --json"},
	{Name: "hosts.inspect", Command: "mesh inspect", Args: "HOST", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read full machine notes and current access.", Example: "mesh inspect HOST_ID --json"},
	{Name: "sessions.list", Command: "mesh session list"},
	{Name: "sessions.inspect", Command: "mesh session inspect", Args: "SESSION_ID"},
	{Name: "profile.show", Command: "mesh profile show", Implemented: true, Flags: []Flag{{Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Read this machine’s owner-written notes.", Example: "mesh profile show --json"},
	{Name: "profile.set", Command: "mesh profile set", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--file", Value: pathFlagValue, Required: true, Summary: "editable JSON file, or - for stdin"}, {Name: expectedRevisionFlag, Value: "N", Required: true, Summary: "revision before this write"}}, Summary: "Replace this machine’s notes at an expected revision.", Example: "mesh profile set --file PATH --expected-revision 0 --json"},
	{Name: "profile.edit", Command: "mesh profile edit", Implemented: true, Mutates: true, Summary: "Edit this machine’s notes with VISUAL or EDITOR, for humans only.", Example: "mesh profile edit"},
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
	if len(words) >= 2 && words[0] == "device" && words[1] == "scope" {
		return "scope.list"
	}
	// The implemented profile group attributes usage failures to its read operation.
	if len(words) > 0 && words[0] == "profile" {
		return "profile.show"
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
		if value {
			value = false
			continue
		}
		if arg == "--" {
			break
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
	case "--leave-key", "--detach-key", "--host", "--timeout", "--file", expectedRevisionFlag, keyDirFlag, identityFlag, "--permissions", "--expires-at", "--expires-in", "--section", "--session", "--cursor", "--limit":
		return true
	}
	return false
}
