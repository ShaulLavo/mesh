package agentresume

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureRecipe(provider Provider) Recipe {
	return Recipe{Version: Version, Launch: Launch{
		Provider: provider, Executable: "/bin/provider", ProviderVersion: "fixture-v1",
		Directory: "/project with spaces", DataRoot: "/provider state",
	}, ConversationID: "opaque conversation; $(literal) `id`", InvocationToken: "test-token",
		RegisteredAt: time.Now(), Lifecycle: Active}
}

func TestResumePreservesIdentityWithoutPromptOrFreshFlags(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			checkResumeArguments(t, provider)
		})
	}
}

func checkResumeArguments(t *testing.T, provider Provider) {
	t.Helper()
	input := []string{"--model", "model-name", "--", "initial prompt must disappear"}
	launch, err := ParseLaunch(provider, "/bin/provider", "fixture-v1", "/project with spaces",
		[]string{"HOME=/home/test"}, input)
	if err != nil {
		t.Fatal(err)
	}
	recipe := fixtureRecipe(provider)
	recipe.Launch = launch
	argv, err := ResumeCommand(recipe)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/provider", "resume", "--model", "model-name", "--cd", launch.Directory, "--", recipe.ConversationID}
	if provider == Claude {
		want = []string{"/bin/provider", "--model", "model-name", "--resume=" + recipe.ConversationID}
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("resume = %#v, want %#v", argv, want)
	}
	serialized, err := json.Marshal(recipe)
	if err != nil || strings.Contains(string(serialized), "initial prompt") {
		t.Fatalf("recipe retained prompt or failed encoding: %s %v", serialized, err)
	}
}

func TestNativeArgumentBoundariesThroughARealProcess(t *testing.T) {
	if os.Getenv("MESH_TEST_AGENT_ARGV") == "1" {
		args := os.Args
		for i, arg := range args {
			if arg == "--" {
				_ = json.NewEncoder(os.Stdout).Encode(args[i+1:])
				os.Exit(0)
			}
		}
		os.Exit(1)
	}
	for _, provider := range []Provider{Codex, Claude} {
		checkRealArgv(t, provider)
	}
}

func checkRealArgv(t *testing.T, provider Provider) {
	t.Helper()
	recipe := fixtureRecipe(provider)
	recipe.ConversationID = "-opaque id; $(touch must-not-exist) `literal`"
	argv, err := ResumeCommand(recipe)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestNativeArgumentBoundariesThroughARealProcess$", "--"}, argv...)...) //nolint:gosec // self-exec of this test binary proves argument boundaries without a shell
	command.Env = append(os.Environ(), "MESH_TEST_AGENT_ARGV=1")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(output, &got); err != nil || !reflect.DeepEqual(got, argv) {
		t.Fatalf("argv lost boundaries: %s %v", output, err)
	}
}

func TestUnsupportedLaunchOptionsDisableCapture(t *testing.T) {
	cases := []struct {
		provider Provider
		argv     []string
	}{
		{Codex, []string{"--remote", "unix:///server.sock"}},
		{Codex, []string{"--add-dir", "relative", "--cd", "other-project"}},
		{Codex, []string{"-c", "api_key=must-not-save"}},
		{Codex, []string{"--dangerously-bypass-hook-trust"}},
		{Claude, []string{"--print", "secret prompt"}},
		{Claude, []string{"--settings", `{"env":{"API_KEY":"must-not-save"}}`}},
		{Claude, []string{"--agent", "custom"}},
		{Claude, []string{"--worktree", "new-tree"}},
	}
	for _, tc := range cases {
		_, err := ParseLaunch(tc.provider, "/bin/provider", "version", "/project", []string{"HOME=/home/test"}, tc.argv)
		if err == nil {
			t.Errorf("accepted unsupported invocation %s %v", tc.provider, tc.argv)
		}
	}
}

func TestLaunchOptionsFitRecoveryCommandBudget(t *testing.T) {
	options := make([]string, 0, 20)
	for range 10 {
		options = append(options, "--model", strings.Repeat("m", MaxFieldBytes))
	}
	_, err := ParseLaunch(Claude, "/bin/provider", "version", "/project", []string{"HOME=/home/test"}, options)
	if err == nil {
		t.Fatal("accepted options larger than the recovery command budget")
	}
}

func TestLaunchKeepsSupportedPermissionsAndStorage(t *testing.T) {
	launch, err := ParseLaunch(Codex, "/bin/codex", "codex-cli 0.153.4", "/original",
		[]string{"HOME=/home/test", "CODEX_HOME=/custom state"},
		[]string{"--profile=work", "--sandbox", "read-only", "--ask-for-approval=on-request", "--cd", "../worktree", "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--profile", "work", "--sandbox", "read-only", "--ask-for-approval", "on-request"}
	if launch.DataRoot != "/custom state" || launch.Directory != "/worktree" || !reflect.DeepEqual(launch.Options, want) {
		t.Fatalf("launch lost context: %+v", launch)
	}
	recipe := fixtureRecipe(Codex)
	recipe.Launch = launch
	inherited := []string{"HOME=/home/test", "CODEX_HOME=/wrong", "MESH_AGENT_TOKEN=old", "MESH_AGENT_SOCKET=old", "PATH=/bin"}
	resumed, err := ResumeEnv(recipe, inherited)
	if err != nil || envValue(resumed, "CODEX_HOME") != launch.DataRoot || envValue(resumed, "MESH_AGENT_TOKEN") != "" || envValue(resumed, "PATH") != "/bin" {
		t.Fatalf("bad resumed environment: %v %v", resumed, err)
	}
	if inherited[1] != "CODEX_HOME=/wrong" {
		t.Fatal("ResumeEnv mutated caller environment")
	}
}

func TestHookFiltersDataAndRecognizesOnlyPrimaryLifecycle(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		event, err := DecodeEvent(provider, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"opaque-id","cwd":"/project","source":"compact","prompt":"must-not-save","transcript_path":"/private","environment":{"KEY":"must-not-save"}}`))
		if err != nil || event.Kind != Start || event.ConversationID != "opaque-id" || event.Subagent {
			t.Fatalf("decode %s: %+v %v", provider, event, err)
		}
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), "must-not-save") || strings.Contains(string(encoded), "private") {
			t.Fatalf("retained provider context: %s", encoded)
		}
	}
	for _, subagentField := range []string{`"agent_id":"child"`, `"agent_type":"Explore"`, `"parent_session_id":"parent"`} {
		event, err := DecodeEvent(Codex, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"parent-id","cwd":"/project",`+subagentField+`}`))
		if err != nil || !event.Subagent {
			t.Errorf("subagent accepted as primary: %+v %v", event, err)
		}
	}
}

func TestMalformedAndOversizedHooksFailClosed(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `{"hook_event_name":"SessionStart","session_id":7,"cwd":"/tmp"}`,
		`{"hook_event_name":"SessionStart","session_id":"id\u001b","cwd":"/tmp"}`,
		`{"hook_event_name":"SessionStart","session_id":"id","cwd":"relative"}`,
		`{"hook_event_name":"SessionStart","session_id":"id","cwd":"/tmp"} {}`,
		strings.Repeat(" ", MaxEventBytes+1),
	} {
		if _, err := DecodeEvent(Claude, strings.NewReader(raw)); err == nil {
			t.Errorf("accepted invalid hook %.100s", raw)
		}
	}
}

func TestSavedRecipesCannotSmuggleAdditionalCommands(t *testing.T) {
	cases := [][]string{{"--last"}, {"--resume", "different-id"}, {"--model", "-another-option"}, {"-m", "model"}, {"--settings", "secret"}}
	for _, options := range cases {
		recipe := fixtureRecipe(Claude)
		recipe.Options = options
		if err := ValidateRecipe(recipe); err == nil {
			t.Errorf("accepted unsafe persisted options %v", options)
		}
	}
}

func TestAvailabilityDoesNotSubstituteDirectory(t *testing.T) {
	root := t.TempDir()
	recipe := fixtureRecipe(Codex)
	recipe.Directory, recipe.DataRoot, recipe.Executable = root, root, os.Args[0]
	if err := CheckAvailable(recipe); err != nil {
		t.Fatal(err)
	}
	recipe.Directory = filepath.Join(root, "deleted-worktree")
	if err := CheckAvailable(recipe); err == nil {
		t.Fatal("missing worktree accepted")
	}
}

func TestStableHookDefinitionHasNoInvocationData(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		first, err := StableHookFragment(provider, "/path with 'quote/mesh")
		if err != nil {
			t.Fatal(err)
		}
		second, _ := StableHookFragment(provider, "/path with 'quote/mesh")
		if string(first) != string(second) || strings.Contains(string(first), "MESH_AGENT_TOKEN") || !json.Valid(first) {
			t.Errorf("unstable or invalid hooks: %s", first)
		}
	}
}
