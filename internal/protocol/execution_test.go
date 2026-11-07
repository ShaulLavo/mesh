package protocol

import (
	"strings"
	"testing"
)

func TestExecutionRawValidationPreservesEvidence(t *testing.T) {
	valid := `{"type":"execution.create","requestId":"r","requestKey":"job","kind":"pty","command":["/bin/true"],"cwd":"/work"}`
	for _, change := range []string{`,"env":["SECRET=x"]`, `,"owner":"app"`, `,"principal":"other"`, `,"reservedId":"7K3D"`, `,"instance":"forged"`, `,"stdin":true`, `,"depth":1`, `,"command":["different"]`, `,"type":"session.create"`} {
		if _, err := DecodeControl([]byte(strings.TrimSuffix(valid, "}") + change + "}")); err == nil {
			t.Errorf("accepted evidence loss: %s", change)
		}
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"/bin/true"`, `"\ud800"`, 1),
		strings.Replace(valid, `"/bin/true"`, `"\udfff"`, 1),
		strings.Replace(valid, `"/bin/true"`, `"bad\u0000value"`, 1),
		strings.Replace(valid, `"/bin/true"`, `null`, 1),
		strings.Replace(valid, `["/bin/true"]`, `null`, 1),
		strings.Replace(valid, `"cwd":"/work"`, `"cwd":5`, 1),
		strings.Replace(valid, `"kind":"pty"`, `"kind":true`, 1),
		strings.TrimSuffix(valid, "}") + `,"rows":null}`,
		strings.TrimSuffix(valid, "}") + `,"term":"` + strings.Repeat("x", 41<<10) + `"}`,
		strings.Replace(valid, "/bin/true", "bad\xffvalue", 1),
	} {
		if _, err := DecodeControl([]byte(invalid)); err == nil {
			t.Errorf("accepted invalid raw request: %.120s", invalid)
		}
	}
	for _, good := range []string{valid, strings.Replace(valid, `"/bin/true"`, `"\ud83d\ude00"`, 1), strings.Replace(valid, `"/bin/true"`, `"\\ud800"`, 1)} {
		if _, err := DecodeControl([]byte(good)); err != nil {
			t.Errorf("rejected valid raw request %s: %v", good, err)
		}
	}
	// Legacy requests retain their existing permissive decoder.
	if _, err := DecodeControl([]byte(`{"type":"session.create","unknown":true}`)); err != nil {
		t.Fatal(err)
	}
}
func TestExecutionRequestRejectsCreateFields(t *testing.T) {
	for _, field := range []string{`"cwd":"/work"`, `"command":[]`, `"cols":80`, `"kind":"pty"`, `"stdin":false`} {
		raw := `{"type":"execution.request","requestId":"r","requestKey":"job",` + field + `}`
		if _, err := DecodeControl([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestExecutionDiscriminatorAliasesCannotBypassRawBoundary(t *testing.T) {
	for _, name := range []string{"Type", "TYPE", "tYpE"} {
		raw := `{"` + name + `":"execution.create","requestId":"r","requestKey":"job","kind":"pty","command":["/bin/true"],"cwd":"/work","env":["injected"]}`
		if _, err := DecodeControl([]byte(raw)); err == nil {
			t.Fatalf("decoder accepted execution discriminator alias %q", name)
		}
	}
}
