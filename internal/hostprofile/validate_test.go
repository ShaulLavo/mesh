package hostprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditableValidation(t *testing.T) {
	bad := []string{`null`, `[]`, `{"schemaVersion":2}`, `{"schemaVersion":1,"unknown":1}`, `{"SchemaVersion":1}`, `{"schemaVersion":1,"SchemaVersion":1}`, `{"schemaVersion":1,"description":"a","description":"b"}`, `{"schemaVersion":1,"description":"\u202e"}`, `{"schemaVersion":1,"description":"\n"}`, `{"schemaVersion":1,"description":"\u0085"}`, `{"schemaVersion":1,"instructions":"\r"}`, `{"schemaVersion":1,"labels":{"A":"x"}}`, `{"schemaVersion":1,"labels":{"key":"a","KEY":"b"}}`, `{"schemaVersion":1} {}`, `{"schemaVersion":1,"description":null}`, `{"schemaVersion":1,"labels":{"key":null}}`, `{"schemaVersion":1,"warnings":[null]}`, `{"schemaVersion":1,"instructions":"\ud800"}`, "{\"schemaVersion\":1,\"description\":\"\xff\"}", strings.Repeat("[", 10) + strings.Repeat("]", 10)}
	for _, value := range bad {
		if _, err := ParseEditable([]byte(value), HostLimits); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	p, err := ParseEditable([]byte(`{"schemaVersion":1,"instructions":"one\ntwo\t三"}`), HostLimits)
	if err != nil || p.Instructions != "one\ntwo\t三" || p.Labels == nil || p.Warnings == nil {
		t.Fatal(p, err)
	}
}

func TestEditableLimits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Editable)
	}{
		{"description", func(p *Editable) { p.Description = strings.Repeat("é", 513) }},
		{"instructions", func(p *Editable) { p.Instructions = strings.Repeat("x", (16<<10)+1) }},
		{"label key", func(p *Editable) { p.Labels = map[string]string{strings.Repeat("k", 65): "v"} }},
		{"label value", func(p *Editable) { p.Labels = map[string]string{"k": strings.Repeat("v", 257)} }},
		{"warnings", func(p *Editable) { p.Warnings = make([]string, 17) }},
		{"warning", func(p *Editable) { p.Warnings = []string{strings.Repeat("x", 513)} }},
	}
	for _, tc := range cases {
		p := editable("")
		tc.mutate(&p)
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseEditable(data, HostLimits); err == nil {
			t.Errorf("accepted excess %s", tc.name)
		}
	}
	if _, err := ParseEditable([]byte(strings.Repeat(" ", (32<<10)+1)), HostLimits); err == nil {
		t.Fatal("encoded limit accepted")
	}
	p := editable(strings.Repeat("<", 6000))
	data, _ := json.Marshal(p)
	if len(data) <= HostLimits.Encoded {
		t.Fatal("bad fixture")
	}
	if _, err := ParseEditable(data, HostLimits); err == nil {
		t.Fatal("escaped encoded excess accepted")
	}
}

func FuzzParseEditable(f *testing.F) {
	for _, seed := range []string{`{"schemaVersion":1}`, `{"schemaVersion":1,"instructions":"hello\nthere"}`, `{"schemaVersion":1,"labels":{"a":"b","A":"c"}}`, `{"schemaVersion":1,"description":"\ud800"}`, "\xff", strings.Repeat("[", 10) + strings.Repeat("]", 10)} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseEditable(data, HostLimits)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseEditable(encoded, HostLimits); err != nil {
			t.Fatalf("noncanonical accepted input: %v", err)
		}
	})
}
