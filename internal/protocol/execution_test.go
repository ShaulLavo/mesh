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

func TestExecutionReadWaitBelongsToLogsAndWait(t *testing.T) {
	base := `{"requestId":"r","sessionId":"7K3D","instance":"i","timeoutMillis":30000,"type":"`
	for _, kind := range []string{TypeExecutionLogs, TypeExecutionWait} {
		if _, err := DecodeControl([]byte(base + kind + `"}`)); err != nil {
			t.Errorf("%s rejected its caller wait: %v", kind, err)
		}
	}
	if _, err := DecodeControl([]byte(base + TypeExecutionSignal + `"}`)); err == nil {
		t.Error("signal accepted a caller wait")
	}
	// An explicit value is a request to wait, so it must be a real wait.
	for _, kind := range []string{TypeExecutionLogs, TypeExecutionWait} {
		for _, bad := range []string{`0`, `-1`, `60001`, `"1"`, `null`, `1.5`} {
			raw := `{"requestId":"r","sessionId":"7K3D","instance":"i","timeoutMillis":` + bad + `,"type":"` + kind + `"}`
			if _, err := DecodeControl([]byte(raw)); err == nil {
				t.Errorf("%s accepted timeoutMillis %s", kind, bad)
			}
		}
		for _, good := range []string{`1`, `60000`} {
			raw := `{"requestId":"r","sessionId":"7K3D","instance":"i","timeoutMillis":` + good + `,"type":"` + kind + `"}`
			if _, err := DecodeControl([]byte(raw)); err != nil {
				t.Errorf("%s rejected timeoutMillis %s: %v", kind, good, err)
			}
		}
	}
}

func TestExecutionCreateInlineWaitBounds(t *testing.T) {
	create := `{"type":"execution.create","requestId":"r","requestKey":"job","kind":"pipe","command":["/bin/true"],"cwd":"/work"`
	for _, good := range []string{`,"waitMillis":1`, `,"waitMillis":60000`, `,"waitMillis":1000,"outputBytes":1`, `,"waitMillis":1000,"outputBytes":1048576`} {
		if _, err := DecodeControl([]byte(create + good + "}")); err != nil {
			t.Errorf("create rejected %s: %v", good, err)
		}
	}
	for _, bad := range []string{`,"waitMillis":0`, `,"waitMillis":60001`, `,"waitMillis":null`, `,"waitMillis":"5"`, `,"outputBytes":10`, `,"waitMillis":10,"outputBytes":0`, `,"waitMillis":10,"outputBytes":1048577`} {
		if _, err := DecodeControl([]byte(create + bad + "}")); err == nil {
			t.Errorf("create accepted %s", bad)
		}
	}
	// Only a create waits inline. Each baseline is valid on its own, so a
	// rejection comes from the inline field.
	for _, base := range []string{
		`{"type":"execution.wait","requestId":"r","sessionId":"7K3D","instance":"i"`,
		`{"type":"execution.logs","requestId":"r","sessionId":"7K3D","instance":"i","stream":"stdout"`,
		`{"type":"execution.request","requestId":"r","requestKey":"job"`,
	} {
		if _, err := DecodeControl([]byte(base + "}")); err != nil {
			t.Fatalf("baseline rejected: %s: %v", base, err)
		}
		for _, field := range []string{`,"waitMillis":10`, `,"outputBytes":10`} {
			if _, err := DecodeControl([]byte(base + field + "}")); err == nil {
				t.Errorf("accepted %s on %s", field, base)
			}
		}
	}
}
