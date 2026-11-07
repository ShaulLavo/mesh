package identity

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrantReceiptProcess(t *testing.T) {
	if os.Getenv("MESH_GRANT_HELPER") == "" {
		return
	}
	state, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	id, mode := os.Getenv("MESH_GRANT_HELPER_ID"), os.Getenv("MESH_GRANT_HELPER_MODE")
	publications := 0
	publisher := func(state, path string, next []byte) error {
		publications++
		return grantProcessPublication(state, path, next, mode)
	}
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, mode != "revoke", publisher)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GRANT-RESULT changed=%v publications=%d", changed, publications)
}

func grantProcessPublication(state, path string, next []byte, mode string) error {
	if mode == "exit-invalidated" {
		os.Exit(23)
	}
	err := publishGrantsWithSync(state, path, next, func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			return grantFixtureError(err)
		}
		if info.IsDir() && mode == "exit-renamed" {
			os.Exit(23)
		}
		return grantFixtureError(file.Sync())
	})
	if err == nil && mode == "exit-durable" {
		os.Exit(23)
	}
	return grantFixtureError(err)
}

func runGrantProcess(t *testing.T, state, id, mode string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: executable, Args: []string{executable, "-test.run=^TestGrantReceiptProcess$", "-test.v"}}
	command.Dir = state
	command.Env = append(os.Environ(), "MESH_GRANT_HELPER=1", "MESH_GRANT_HELPER_ID="+id, "MESH_GRANT_HELPER_MODE="+mode)
	output, err := command.CombinedOutput()
	if !strings.HasPrefix(mode, "exit-") {
		if err != nil {
			t.Fatalf("helper: %v\n%s", err, output)
		}
		return string(output)
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 23 {
		t.Fatalf("exit helper: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("GRANT-RESULT")) {
		t.Fatal("interrupted mutation returned a successful result")
	}
	return string(output)
}

func TestGrantAcknowledgementSurvivesFreshProcess(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	pinGrantPolicy(t, state)
	before, err := os.Stat(filepath.Join(state, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	output := runGrantProcess(t, state, id, "approve")
	if !strings.Contains(output, "GRANT-RESULT changed=false publications=0") {
		t.Fatalf("fresh helper did not reuse receipt: %s", output)
	}
	after, err := os.Stat(filepath.Join(state, "authorized_keys"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("fresh process replaced policy: %v", err)
	}
	other := grantActor(t)
	runGrantProcess(t, state, other, "approve")
	if changed, err := ApproveDeviceChanged(t.Context(), state, id); changed || err != nil {
		t.Fatalf("after intervening writer: changed=%v, error=%v", changed, err)
	}
	if !GrantedIdentity(state, other) {
		t.Fatal("cached policy lost intervening grant")
	}
	bound, ok := BindIdentity(state, id)
	if !ok {
		t.Fatal("first grant missing")
	}
	runGrantProcess(t, state, id, "revoke")
	if bound() {
		t.Fatal("intervening process revocation ignored")
	}
	if changed, err := ApproveDeviceChanged(t.Context(), state, id); !changed || err != nil {
		t.Fatalf("reapproval: changed=%v, error=%v", changed, err)
	}
	if bound() || !GrantedIdentity(state, other) {
		t.Fatal("reapproval healed old binding or lost unrelated grant")
	}
	independent := t.TempDir()
	output = runGrantProcess(t, independent, id, "approve")
	if !strings.Contains(output, "GRANT-RESULT changed=true publications=1") {
		t.Fatalf("independent state reused receipt: %s", output)
	}
}

func TestGrantInterruptedPublicationNeedsIndependentPublication(t *testing.T) {
	for _, phase := range []string{"exit-invalidated", "exit-renamed", "exit-durable"} {
		t.Run(phase, func(t *testing.T) { checkGrantInterruptedPublication(t, phase) })
	}
}

func checkGrantInterruptedPublication(t *testing.T, phase string) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	other := grantActor(t)
	runGrantProcess(t, state, other, phase)
	if len(grantBytes(t, state, "device-grants.lock")) != 0 {
		t.Fatal("interrupted publication left a receipt")
	}
	if GrantedIdentity(state, other) != (phase != "exit-invalidated") {
		t.Fatal("unexpected rename boundary")
	}
	output := runGrantProcess(t, state, id, "approve")
	if !strings.Contains(output, "GRANT-RESULT changed=false publications=1") {
		t.Fatalf("fresh process skipped independent publication: %s", output)
	}
}
