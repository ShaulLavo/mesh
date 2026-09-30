package testenv

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestForProcessRunsWithOnlySelectedEnvironment(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "HTTP_PROXY", "https_proxy", "UNRELATED_CALLER_SETTING"} {
		t.Setenv(name, "fixture-caller-value")
	}
	t.Setenv("TERM", "fixture-term")
	t.Setenv("LANG", "C")
	home := t.TempDir()
	command := exec.Command("env")
	command.Env = ForProcess(home)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	want := map[string]string{"PATH": os.Getenv("PATH"), "HOME": home, "TERM": "fixture-term", "LANG": "C"}
	if value, ok := os.LookupEnv("TMPDIR"); ok {
		want["TMPDIR"] = value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("fixture environment differs from the allow-list and isolated home")
	}
}
