package agentapi

import "strings"

const fleetFlag = "--fleet"
const timeoutFlag = "--timeout"
const jsonFlag = "--json"
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
	{Name: "services.list", Command: "mesh serve ls", Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for live host queries"}}, Network: true, Implemented: true, Summary: "List live and cached services without opening their URLs.", Example: "mesh serve ls --json"},
	{Name: "hosts.list", Command: "mesh hosts", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read machine context without waking machines.", Example: "mesh hosts --json"},
	{Name: "hosts.inspect", Command: "mesh inspect", Args: "HOST", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read full machine notes and current access.", Example: "mesh inspect HOST_ID --json"},
	{Name: "sessions.list", Command: "mesh session list", Implemented: true, Network: true, Flags: sessionReadFlags(true), Summary: "Read a bounded page of sessions without attaching.", Example: "mesh session list --host HOST_ID --json"},
	{Name: "sessions.inspect", Command: "mesh session inspect", Args: "SESSION_ID", Implemented: true, Network: true, Flags: sessionReadFlags(false), Summary: "Read live or saved session details without attaching.", Example: "mesh session inspect --host HOST_ID SESSION_ID --json"},
	{Name: "profile.show", Command: "mesh profile show", Implemented: true, Flags: []Flag{{Name: fleetFlag, Summary: "read the local signed fleet replica"}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Read this machine’s owner-written notes.", Example: "mesh profile show --json"},
	{Name: "profile.set", Command: "mesh profile set", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Summary: "publish signed notes to saved fleet members"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--file", Value: "PATH", Required: true, Summary: "editable JSON file, or - for stdin"}, {Name: "--expected-revision", Value: "N", Required: true, Summary: "revision before this write"}}, Summary: "Replace this machine’s notes at an expected revision.", Example: "mesh profile set --file PATH --expected-revision 0 --json"},
	{Name: "profile.edit", Command: "mesh profile edit", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Summary: "edit signed fleet notes"}}, Summary: "Edit this machine’s notes with VISUAL or EDITOR, for humans only.", Example: "mesh profile edit"},
	{Name: "profile.status", Command: "mesh profile status", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Required: true, Summary: "read fleet delivery state"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--retry", Summary: "reset pending and stale delivery age"}}, Summary: "Read local fleet delivery state without contacting members.", Example: "mesh profile status --fleet --json"},
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
	// The implemented profile group attributes usage failures to its read operation.
	if len(words) > 0 && words[0] == "profile" {
		return "profile.show"
	}
	if len(words) > 0 && words[0] == "session" {
		return "sessions.list"
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
	case "--leave-key", "--detach-key", "--host", "--timeout", "--file", "--expected-revision", "--limit", "--page":
		return true
	}
	return false
}

func sessionReadFlags(list bool) []Flag {
	flags := []Flag{{Name: "--host", Value: "HOST", Required: true, Summary: "machine name or exact host ID"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline from 100ms to 60s"}}
	if !list {
		return append(flags, Flag{Name: "--preview", Summary: "include a preview at most 80 by 24"})
	}
	return append(flags, Flag{Name: "--all", Summary: "include ended sessions"}, Flag{Name: "--detail", Summary: "include launch command and directory"}, Flag{Name: "--limit", Value: "N", Summary: "page size from 1 to 500"}, Flag{Name: "--page", Value: "TOKEN", Summary: "nextPage token from this host"})
}
