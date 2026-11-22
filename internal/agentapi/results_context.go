package agentapi

import "time"

// ExecutionContext is one context as an agent sees it. It names environment
// variables and a digest of their values, never the values.
type ExecutionContext struct {
	HostID        string     `json:"hostId"`
	ContextID     string     `json:"contextId"`
	Handle        string     `json:"handle"`
	RequestKey    string     `json:"requestKey"`
	State         string     `json:"state"`
	Revision      uint64     `json:"revision,string"`
	Cwd           string     `json:"cwd"`
	Shell         string     `json:"shell"`
	ShellPath     string     `json:"shellPath,omitempty"`
	Label         string     `json:"label,omitempty"`
	EnvNames      []string   `json:"envNames"`
	EnvDigest     string     `json:"envDigest"`
	RunningJobs   int        `json:"runningJobs"`
	UncertainJobs int        `json:"uncertainJobs"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    time.Time  `json:"lastUsedAt"`
	ClosedAt      *time.Time `json:"closedAt,omitempty"`
	ExpiresAt     time.Time  `json:"expiresAt"`
}

// ExecutionContexts is a list result.
type ExecutionContexts struct {
	HostID     string             `json:"hostId"`
	Contexts   []ExecutionContext `json:"contexts"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// ApplicationIdentity describes a local caller identity without its secret.
type ApplicationIdentity struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
	Path        string    `json:"path"`
}

// ApplicationIdentities is a list result.
type ApplicationIdentities struct {
	Identities []ApplicationIdentity `json:"identities"`
}

// applicationValueFlag lists the value flags of the identity and context
// options, so early JSON routing skips their operands.
func applicationValueFlag(arg string) bool {
	switch arg {
	case contextFlag, "--shell-command", "--env", labelFlag, "--shell", "--set-env", "--unset-env":
		return true
	}
	return false
}

const contextFlag = "--context"

// applicationExecutionFlags are the session job flags that act as one
// application caller.
func applicationExecutionFlags() []Flag {
	return []Flag{{Name: identityFlag, Value: nameValue, Summary: "act as this local application identity"}, {Name: contextFlag, Value: contextValue, Summary: "with --identity, the job's context"}}
}

const (
	contextHostFlag = "--host"
	requestKeyFlag  = "--request-key"
	cwdFlag         = "--cwd"
	labelFlag       = "--label"
	nameValue       = "NAME"
	contextValue    = "CONTEXT"
	keyValue        = "KEY"
	textValue       = "TEXT"
)

func contextFlags(extra ...Flag) []Flag {
	return append([]Flag{{Name: jsonFlag, Required: true, Summary: jsonFlagSummary}, {Name: identityFlag, Value: nameValue, Required: true, Summary: "local application identity"}, {Name: contextHostFlag, Value: hostValue, Summary: "online target; implied by a context handle"}, {Name: timeoutFlag, Value: "D", Summary: "finite request deadline"}}, extra...)
}

// applicationOperations are the identity and context commands.
var applicationOperations = []Operation{
	{Name: "mcp.identity.create", Command: "mesh mcp identity create", Args: nameValue, Implemented: true, Mutates: true, Flags: []Flag{{Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "Create a private application credential for one agent frontend.", Example: "mesh mcp identity create codex --json"},
	{Name: "mcp.identity.list", Command: "mesh mcp identity ls", Implemented: true, Flags: []Flag{{Name: jsonFlag, Summary: jsonFlagSummary}}, Summary: "List local application identities without their secrets.", Example: "mesh mcp identity ls --json"},
	{Name: "contexts.open", Command: "mesh context open", Implemented: true, Network: true, Mutates: true, Flags: contextFlags(Flag{Name: requestKeyFlag, Value: keyValue, Required: true, Summary: "saved logical request key"}, Flag{Name: cwdFlag, Value: pathFlagValue, Required: true, Summary: "absolute directory on the target"}, Flag{Name: "--shell", Value: "SHELL", Summary: "login, sh or none"}, Flag{Name: "--env", Value: "NAME=VALUE", Summary: "overlay variable, repeatable"}, Flag{Name: labelFlag, Value: textValue, Summary: "display label"}), Summary: "Open a durable working context on a host for one application.", Example: "mesh context open --host HOST_ID --identity codex --request-key ctx-1 --cwd /work --json"},
	{Name: "contexts.get", Command: "mesh context get", Args: contextValue, Implemented: true, Network: true, Flags: contextFlags(), Summary: "Read one context owned by the application.", Example: "mesh context get --identity codex mc1.HOST_ID.CONTEXT_ID --json"},
	{Name: "contexts.list", Command: "mesh context ls", Implemented: true, Network: true, Flags: contextFlags(), Summary: "List the application's contexts on a host.", Example: "mesh context ls --host HOST_ID --identity codex --json"},
	{Name: "contexts.update", Command: "mesh context update", Args: contextValue, Implemented: true, Network: true, Mutates: true, Flags: contextFlags(Flag{Name: expectedRevisionFlag, Value: "N", Required: true, Summary: "revision before this change"}, Flag{Name: cwdFlag, Value: pathFlagValue, Summary: "new absolute directory"}, Flag{Name: "--set-env", Value: "NAME=VALUE", Summary: "set an overlay variable, repeatable"}, Flag{Name: "--unset-env", Value: nameValue, Summary: "remove an overlay variable, repeatable"}, Flag{Name: labelFlag, Value: textValue, Summary: "new display label"}), Summary: "Change a context's directory, environment or label at an expected revision.", Example: "mesh context update --identity codex mc1.HOST_ID.CONTEXT_ID --expected-revision 1 --cwd /work/app --json"},
	{Name: "contexts.close", Command: "mesh context close", Args: contextValue, Implemented: true, Network: true, Mutates: true, Flags: contextFlags(Flag{Name: expectedRevisionFlag, Value: "N", Summary: "fail if the context changed"}), Summary: "Stop new jobs in a context; running jobs keep going.", Example: "mesh context close --identity codex mc1.HOST_ID.CONTEXT_ID --json"},
	{Name: "contexts.receipt", Command: "mesh context receipt", Implemented: true, Network: true, Flags: contextFlags(Flag{Name: requestKeyFlag, Value: keyValue, Required: true, Summary: "request key used to open"}), Summary: "Look up a context by the key that opened it, after a lost reply.", Example: "mesh context receipt --host HOST_ID --identity codex --request-key ctx-1 --json"},
}
