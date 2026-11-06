package hostprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestEscapedSurrogatesAndReplacementCharacter(t *testing.T) {
	for _, text := range []string{`\ud800`, `\udbff`, `\udc00`, `\udfff`, `\ud800x`, `\ud800\ud800`, `\udfff\ud800`} {
		if _, err := ParseEditable([]byte(`{"schemaVersion":1,"instructions":"`+text+`"}`), HostLimits); err == nil {
			t.Errorf("silently replaced %s", text)
		}
	}
	for _, tc := range []struct{ source, want string }{
		{`\ud800\udc00`, "\U00010000"},
		{`\udbff\udfff`, "\U0010ffff"},
		{`\ud83d\ude00`, "😀"},
		{`\ufffd`, "�"},
		{"�", "�"},
		{`\\ud800`, `\ud800`},
	} {
		p, err := ParseEditable([]byte(`{"schemaVersion":1,"instructions":"`+tc.source+`"}`), HostLimits)
		if err != nil || p.Instructions != tc.want {
			t.Errorf("%s: %q %v", tc.source, p.Instructions, err)
		}
	}
}

func TestEditableBoundaryValues(t *testing.T) {
	values := []Editable{editable(strings.Repeat("é", 8192)), {SchemaVersion: 1, Description: strings.Repeat("x", 1024)}, {SchemaVersion: 1, Labels: map[string]string{strings.Repeat("k", 64): strings.Repeat("v", 256)}}, {SchemaVersion: 1, Warnings: make([]string, 16)}}
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseEditable(encoded, HostLimits); err != nil {
			t.Fatal(err)
		}
	}
	padded := append([]byte(`{"schemaVersion":1}`), bytes.Repeat([]byte(" "), HostLimits.Encoded-len(`{"schemaVersion":1}`))...)
	if _, err := ParseEditable(padded, HostLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEditable(append(padded, ' '), HostLimits); err == nil {
		t.Fatal("accepted encoded limit plus one")
	}
	labels := map[string]string{}
	for i := 0; i < 33; i++ {
		labels[fmt.Sprintf("label-%d", i)] = "value"
	}
	encoded, err := json.Marshal(Editable{SchemaVersion: 1, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEditable(encoded, HostLimits); err == nil {
		t.Fatal("accepted 33 labels")
	}
}

func TestStrictDepthAndAllControlRanges(t *testing.T) {
	for _, depth := range []int{8, 9} {
		decoder := json.NewDecoder(strings.NewReader(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)))
		err := CheckJSON(decoder, 0, 8)
		if (err != nil) != (depth > 8) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
	for _, r := range []rune{0, 0x1f, 0x7f, 0x80, 0x9f, 0x2028, 0x2029, 0x202a, 0x202e, 0x2066, 0x2069} {
		encoded, err := json.Marshal(editable(string(r)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseEditable(encoded, HostLimits); err == nil {
			t.Errorf("accepted control U+%04X", r)
		}
	}
}
