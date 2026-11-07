package domainpolicy

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Each helper is a real process with one immutable policy initialization. The
// consumer registry is never reset to model closing or reopening a command.
func TestConsumerGenerationStartup(t *testing.T) {
	if mode := os.Getenv("MESH_CONSUMER_GENERATION_CASE"); mode != "" {
		consumerGenerationStartup(t, mode)
		return
	}
	for _, mode := range []string{"logical", "existing", "unavailable", "deployment"} {
		t.Run(mode, func(t *testing.T) { runConsumerGenerationProcess(t, mode) })
	}
}

func runConsumerGenerationProcess(t *testing.T, mode string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^TestConsumerGenerationStartup$")
	if runtime.GOOS != "linux" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command.Path, command.Args[0] = executable, executable
	}
	command.Env = append(os.Environ(), "MESH_CONSUMER_GENERATION_CASE="+mode)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("startup process: %v\n%s", err, output)
	}
}

func consumerGenerationStartup(t *testing.T, mode string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(t.TempDir(), "domains.json")
	before := Policy{Primary: "first.test", Aliases: []string{"second.test"}}
	consumerGenerationSavePolicy(t, path, before)
	if mode == "existing" {
		consumerGenerationPublish(t, state, path, before)
	}
	if mode == "unavailable" {
		if err := os.MkdirAll(state, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, catalogPolicyFile), []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "deployment" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := InitializeDeployment(path, state, true); err != nil {
			t.Fatal(err)
		}
		consumerGenerationFreshDeployment(t, state)
		return
	} else if err := InitializeReadOnlyDeployment(path, state); err != nil {
		t.Fatal(err)
	}
	if mode == "unavailable" {
		consumerGenerationUnavailable(t, state)
		return
	}
	if mode == "logical" {
		if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only startup created state: %v", err)
		}
	}
	// No consumer/cache construction occurs between startup and the cycle.
	after := before
	after.Primary, after.Aliases = "second.test", []string{before.Primary}
	consumerGenerationPublish(t, state, path, after)
	consumerGenerationPublish(t, state, path, before)
	pinned, err := ConsumerCatalogToken(state)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := ReadCatalogPolicy(state)
	if err != nil || pinned.Digest != before.Digest() || observed.CatalogToken == pinned || observed.Generation != pinned.Generation+2 {
		t.Fatalf("lazy consumer rebound startup: pin=%+v observed=%+v err=%v", pinned, observed.CatalogToken, err)
	}
	if Current().Digest() != before.Digest() {
		t.Fatal("domain write changed immutable process policy")
	}
}

func consumerGenerationFreshDeployment(t *testing.T, state string) {
	t.Helper()
	pinned, err := ConsumerCatalogToken(state)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := ReadCatalogPolicy(state)
	if err != nil || pinned != observed.CatalogToken || pinned.Digest != Current().Digest() {
		t.Fatalf("fresh deployment did not pin its published policy: %+v %+v %v", pinned, observed.CatalogToken, err)
	}
}

func consumerGenerationUnavailable(t *testing.T, state string) {
	t.Helper()
	if token, err := ConsumerCatalogToken(state); err == nil || token.Generation != 0 {
		t.Fatalf("unavailable startup admitted a generation: %+v %v", token, err)
	}
	if err := os.Remove(filepath.Join(state, catalogPolicyFile)); err != nil {
		t.Fatal(err)
	}
	if err := WithCatalogGate(t.Context(), state, func(*CatalogGate) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if token, err := ConsumerCatalogToken(state); err == nil || token.Generation != 0 {
		t.Fatalf("late construction admitted an unobserved startup epoch: %+v %v", token, err)
	}
}

func consumerGenerationSavePolicy(t *testing.T, path string, policy Policy) {
	t.Helper()
	contents, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
}

func consumerGenerationPublish(t *testing.T, state, path string, policy Policy) {
	t.Helper()
	err := WithCatalogGate(t.Context(), state, func(gate *CatalogGate) error {
		return consumerGenerationDomainWrite(t, path, policy, gate)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func consumerGenerationDomainWrite(t *testing.T, path string, policy Policy, gate *CatalogGate) error {
	t.Helper()
	err := WithDomainFile(t.Context(), path, func(file *DomainFile) error {
		if err := gate.Prepare(policy.Digest(), nil); err != nil {
			return err
		}
		return file.Save(policy)
	})
	if err != nil {
		return err
	}
	return gate.Settle(func([]string) error { return SettleSavedPolicy(path, policy.Digest()) })
}
