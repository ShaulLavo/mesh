package agentapi

import "strings"

const maxBytesFlag = "--max-bytes"
const sessionFlag = "--session"
const colsFlag = "--cols"
const rowsFlag = "--rows"
const hostValue = "HOST"
const keyDirFlag = "--key-dir"
const identityFlag = "--identity"
const expectedRevisionFlag = "--expected-revision"
const pathFlagValue = "PATH"
const fleetFlag = "--fleet"
const timeoutFlag = "--timeout"
const jsonFlag = "--json"
const hostFlag = "--host"
const limitFlag = "--limit"
const skillDirFlag = "--dir"
const fileFlag = "--file"
const rootOperand = "ROOT"
const skillsRootSummary = "existing skills root"
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
	{Name: "scope.grant", Command: "mesh device scope grant", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: identityFlag, Value: "ID", Required: true}, {Name: "--permissions", Value: "PERMISSIONS", Required: true}, {Name: expectedRevisionFlag, Value: "N", Required: true}, {Name: "--expires-at", Value: "TIME"}, {Name: "--expires-in", Value: "D"}}, Summary: "Grant host-wide metadata or metadata plus session-output locally.", Example: "mesh device scope grant --identity PUBLIC_ID --permissions metadata --expected-revision 0 --json"},
	{Name: "scope.revoke", Command: "mesh device scope revoke", Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: identityFlag, Value: "ID", Required: true}, {Name: expectedRevisionFlag, Value: "N", Required: true}}, Summary: "Remove a scoped key from local target policy.", Example: "mesh device scope revoke --identity PUBLIC_ID --expected-revision 1 --json"},
	{Name: "scope.list", Command: "mesh device scope list", Implemented: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}}, Summary: "Read the local target's scope policy.", Example: "mesh device scope list --json"},
	{Name: "scope.read", Command: "mesh device scope read", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: hostFlag, Value: "ID", Required: true}, {Name: keyDirFlag, Value: pathFlagValue, Required: true}, {Name: "--section", Value: "SECTION"}, {Name: sessionFlag, Value: "ID"}, {Name: cursorFlag, Value: tokenValue}, {Name: limitFlag, Value: "N"}, {Name: timeoutFlag, Value: "D"}}, Summary: "Read a narrow metadata page over explicit-key TLS without fallbacks.", Example: "mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY --json"},
	{Name: "scope.output", Command: "mesh device scope output", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: hostFlag, Value: "ID", Required: true}, {Name: keyDirFlag, Value: pathFlagValue, Required: true}, {Name: sessionFlag, Value: "ID", Required: true}, {Name: "--format", Value: "FORMAT", Required: true}, {Name: maxBytesFlag, Value: "N"}, {Name: colsFlag, Value: "N"}, {Name: rowsFlag, Value: "N"}, {Name: timeoutFlag, Value: "D"}}, Summary: "Read bounded session output over explicit-key TLS without attaching.", Example: "mesh device scope output --host HOST_ID --key-dir KEY_DIRECTORY --session SESSION_ID --format preview --json"},
	{Name: "services.list", Command: "mesh serve ls", Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for live host queries"}}, Network: true, Implemented: true, Summary: "List live and cached services without opening their URLs.", Example: "mesh serve ls --json"},
	{Name: "hosts.list", Command: "mesh hosts", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read machine context without waking machines.", Example: "mesh hosts --json"},
	{Name: "hosts.inspect", Command: "mesh inspect", Args: "HOST", Implemented: true, Network: true, Flags: []Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline for host queries"}, {Name: "--cached", Summary: "do not dial hosts"}}, Summary: "Read full machine notes and current access.", Example: "mesh inspect HOST_ID --json"},
	{Name: "sessions.list", Command: "mesh session list", Implemented: true, Network: true, Flags: sessionReadFlags(true), Summary: "Read a bounded page of sessions without attaching.", Example: "mesh session list --host HOST_ID --json"},
	{Name: "sessions.inspect", Command: "mesh session inspect", Args: sessionIDValue, Implemented: true, Network: true, Flags: sessionReadFlags(false), Summary: "Read live or saved session details without attaching.", Example: "mesh session inspect --host HOST_ID SESSION_ID --json"},
	{Name: "profile.show", Command: "mesh profile show", Implemented: true, Flags: []Flag{{Name: fleetFlag, Summary: "read the local signed fleet replica"}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Read this machine’s owner-written notes.", Example: "mesh profile show --json"},
	{Name: "profile.set", Command: "mesh profile set", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Summary: "publish signed notes to saved fleet members"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--file", Value: pathFlagValue, Required: true, Summary: "editable JSON file, or - for stdin"}, {Name: expectedRevisionFlag, Value: "N", Required: true, Summary: "revision before this write"}}, Summary: "Replace this machine’s notes at an expected revision.", Example: "mesh profile set --file PATH --expected-revision 0 --json"},
	{Name: "profile.edit", Command: "mesh profile edit", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Summary: "edit signed fleet notes"}}, Summary: "Edit this machine’s notes with VISUAL or EDITOR, for humans only.", Example: "mesh profile edit"},
	{Name: "profile.status", Command: "mesh profile status", Implemented: true, Mutates: true, Flags: []Flag{{Name: fleetFlag, Required: true, Summary: "read fleet delivery state"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--retry", Summary: "reset pending and stale delivery age"}}, Summary: "Read local fleet delivery state without contacting members.", Example: "mesh profile status --fleet --json"},
	{Name: "skill.show", Command: "mesh skill show", Implemented: true, Flags: []Flag{{Name: fileFlag, Value: "PATH", Summary: "embedded file to print"}}, Summary: "Print exact embedded skill bytes without reading configuration.", Example: "mesh skill show"},
	{Name: "skill.install", Command: "mesh skill install", Implemented: true, Mutates: true, Flags: []Flag{{Name: skillDirFlag, Value: rootOperand, Required: true, Summary: skillsRootSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Install the managed Mesh skill.", Example: "mesh skill install --dir ROOT --json"},
	{Name: "skill.update", Command: "mesh skill update", Implemented: true, Mutates: true, Flags: []Flag{{Name: skillDirFlag, Value: rootOperand, Required: false, Summary: skillsRootSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Update the managed Mesh skill.", Example: "mesh skill update --dir ROOT --json"},
	{Name: "skill.status", Command: "mesh skill status", Implemented: true, Mutates: false, Flags: []Flag{{Name: skillDirFlag, Value: rootOperand, Required: false, Summary: skillsRootSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Inspect the managed Mesh skill and its registration.", Example: "mesh skill status --dir ROOT --json"},
	{Name: "skill.uninstall", Command: "mesh skill uninstall", Implemented: true, Mutates: true, Flags: []Flag{{Name: skillDirFlag, Value: rootOperand, Required: true, Summary: skillsRootSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Uninstall the managed Mesh skill.", Example: "mesh skill uninstall --dir ROOT --json"},
	{Name: "sessions.create", Command: "mesh session create", Args: "-- command [args...]", Implemented: true, Network: true, Mutates: true, Flags: executionFlags(true), Summary: "Start one keyed detached PTY command on an online host.", Example: "mesh session create --host HOST_ID --kind pty --request-key job-17 --cwd /work --json -- /usr/bin/true"},
	{Name: "sessions.request", Command: "mesh session request", Implemented: true, Network: true, Flags: executionFlags(false), Summary: "Read a durable request receipt without launching work.", Example: "mesh session request --host HOST_ID --request-key job-17 --json"},
	{Name: "sessions.wait", Command: "mesh session wait", Args: sessionIDValue, Implemented: true, Network: true, Mutates: false, Flags: executionControlFlags("wait"), Summary: "Wait for a durable exact-instance result.", Example: "mesh session wait --host HOST_ID 7K3D --instance INSTANCE --json"},
	{Name: "sessions.logs", Command: "mesh session logs", Args: sessionIDValue, Implemented: true, Network: true, Mutates: false, Flags: executionControlFlags("logs"), Summary: "Read exact bounded PTY bytes with independent cursors.", Example: "mesh session logs --host HOST_ID 7K3D --instance INSTANCE --json"},
	{Name: "sessions.signal", Command: "mesh session signal", Args: sessionIDValue, Implemented: true, Network: true, Mutates: true, Flags: executionControlFlags("signal"), Summary: "Signal one exact keyed command instance.", Example: "mesh session signal --host HOST_ID 7K3D --instance INSTANCE --json"},
	{Name: "sessions.kill", Command: "mesh session kill", Args: sessionIDValue, Implemented: true, Network: true, Mutates: true, Flags: executionControlFlags("kill"), Summary: "Stop one exact keyed command instance.", Example: "mesh session kill --host HOST_ID 7K3D --instance INSTANCE --json"},
	{Name: "sessions.remove", Command: "mesh session rm", Args: sessionIDValue, Implemented: true, Network: true, Mutates: true, Flags: executionControlFlags("remove"), Summary: "Remove finished output while retaining the creation receipt.", Example: "mesh session rm --host HOST_ID 7K3D --instance INSTANCE --json"},
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
	if len(words) > 0 && words[0] == "skill" {
		return "skill.status"
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
	case skillDirFlag, "--leave-key", "--detach-key", hostFlag, "--timeout", "--file", expectedRevisionFlag, keyDirFlag, identityFlag, "--permissions", "--expires-at", "--expires-in", "--section", sessionFlag, cursorFlag, limitFlag, "--page", "--request-key", "--kind", "--cwd", "--term", "--format", maxBytesFlag, colsFlag, rowsFlag, "--instance", "--stream", "--signal":
		return true
	}
	return false
}

func sessionReadFlags(list bool) []Flag {
	flags := []Flag{{Name: hostFlag, Value: hostValue, Required: true, Summary: "machine name or exact host ID"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "hard deadline from 100ms to 60s"}}
	if !list {
		return append(flags, Flag{Name: "--preview", Summary: "include a preview at most 80 by 24"})
	}
	return append(flags, Flag{Name: "--all", Summary: "include ended sessions"}, Flag{Name: "--detail", Summary: "include launch command and directory"}, Flag{Name: limitFlag, Value: "N", Summary: "page size from 1 to 500"}, Flag{Name: "--page", Value: tokenValue, Summary: "nextPage token from this host"})
}

func executionFlags(create bool) []Flag {
	flags := []Flag{{Name: hostFlag, Value: hostValue, Required: true, Summary: "online target"}, {Name: "--request-key", Value: "KEY", Required: true, Summary: "saved logical request key"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: timeoutFlag, Value: "D", Summary: "finite request deadline"}}
	if create {
		return append(flags, Flag{Name: "--kind", Value: "pty", Required: true, Summary: "explicit PTY kind"}, Flag{Name: "--cwd", Value: "PATH", Required: true, Summary: "absolute target directory"}, Flag{Name: "--term", Value: "TERM", Summary: "terminal type"}, Flag{Name: colsFlag, Value: "N", Summary: "terminal width"}, Flag{Name: rowsFlag, Value: "N", Summary: "terminal height"})
	}
	return flags
}

func executionControlFlags(kind string) []Flag {
	flags := []Flag{{Name: hostFlag, Value: hostValue, Required: true, Summary: "online target"}, {Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: "--instance", Value: "INSTANCE", Required: true, Summary: "exact creation receipt instance"}, {Name: timeoutFlag, Value: "D", Summary: "finite deadline"}}
	if kind == "logs" {
		flags = append(flags, Flag{Name: "--stream", Value: "pty", Summary: "exact PTY stream"}, Flag{Name: cursorFlag, Value: tokenValue, Summary: "independent output cursor"}, Flag{Name: maxBytesFlag, Value: "N", Summary: "page size from 1 to 1048576"})
	}
	if kind == "signal" {
		flags = append(flags, Flag{Name: "--signal", Value: "SIGNAL", Required: true, Summary: "process group signal"})
	}
	return flags
}

const cursorFlag = "--cursor"
const sessionIDValue = "SESSION_ID"
const tokenValue = "TOKEN"
