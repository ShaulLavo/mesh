package privacy

import (
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestValue(t *testing.T) {
	m := New()
	alias := m.Value("host", "personal-machine")
	if !regexp.MustCompile(`^host-[0-9a-f]{6}$`).MatchString(alias) {
		t.Fatalf("unexpected alias %q", alias)
	}
	for _, mask := range []*Mask{m, New(), {}} {
		if got := mask.Value("host", "personal-machine"); got != alias {
			t.Fatalf("alias changed: %q != %q", got, alias)
		}
	}
	if got := m.Value("host", ""); got != "" {
		t.Fatalf("empty value = %q", got)
	}
	if got := m.Value("path", "personal-machine"); got == alias {
		t.Fatal("different kinds share an alias")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got := m.Value("host", "personal-machine"); got != alias {
				t.Errorf("concurrent alias = %q, want %q", got, alias)
			}
		})
	}
	wg.Wait()
}

func TestNilMask(t *testing.T) {
	var m *Mask
	text := "alice@example.com /home/alice 100.101.102.103 https://private.example"
	if got := m.Text(text); got != text {
		t.Fatalf("nil Text changed text: %q", got)
	}
	if got := m.Value("host", text); got != text {
		t.Fatalf("nil Value changed value: %q", got)
	}
	if got := m.Value("host", ""); got != "" {
		t.Fatalf("nil empty Value = %q", got)
	}
	argv := []string{"/home/alice/bin/run", "secret"}
	if got := m.Command(argv); !reflect.DeepEqual(got, argv) || &got[0] != &argv[0] {
		t.Fatalf("nil Command changed argv: %v", got)
	}
	if m.Command(nil) != nil {
		t.Fatal("nil Command changed nil slice")
	}
}

func TestTextSensitiveExamples(t *testing.T) {
	m := New()
	tests := []struct{ value, kind string }{
		{"alice@example.com", "email"},
		{"first.last+mesh@example.co.uk", "email"},
		{"https://private.example:8443/app?token=secret#fragment", "url"},
		{"ssh://alice@machine/private/path", "url"},
		{"/home/alice/private/project", "path"},
		{"/work/projects/private", "path"},
		{"~/.ssh/id_ed25519", "path"},
		{`C:\Users\alice\private`, "path"},
		{"machine.tail1234.ts.net", "host"},
		{"machine.TAIL1234.TS.NET", "host"},
		{"machine.tailscale.net", "host"},
		{"550e8400-e29b-41d4-a716-446655440000", "uuid"},
		{"alice@machine", "user-host"},
		{"alice@machine.tail1234.ts.net", "email"},
		{"100.101.102.103", "ip"},
		{"127.0.0.1", "ip"},
		{"2001:db8::1", "ip"},
		{"::1", "ip"},
		{"fe80::1234%eth0", "ip"},
		{"[2001:db8::1]", "ip"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			input := "failed (" + tt.value + "), retry"
			want := "failed (" + m.Value(tt.kind, tt.value) + "), retry"
			if got := m.Text(input); got != want {
				t.Fatalf("Text(%q) = %q, want %q", input, got, want)
			}
			if got := New().Text(input); got != want {
				t.Fatalf("new mask alias changed: %q", got)
			}
		})
	}
}

func TestTextPreservesOrdinaryAndANSIText(t *testing.T) {
	m := New()
	for _, text := range []string{
		"", "connection failed: retry in 2s", "build v1.2.3 succeeded",
		"999.999.999.999", "elapsed 12:34", "host-123abc",
		"\x1b[31mconnection failed\x1b[0m",
		"\x1b[38:2:1:2:3:4:5:6mcolored\x1b[0m",
		"\x1b[1;32mready\x1b[0m\x1b[2K\r",
	} {
		if got := m.Text(text); got != text {
			t.Errorf("ordinary text %q changed to %q", text, got)
		}
	}
	input := "\x1b[31m/home/alice/private\x1b[0m"
	if got, want := m.Text(input), "\x1b[31m"+m.Value("path", "/home/alice/private")+"\x1b[0m"; got != want {
		t.Fatalf("ANSI wrapped path = %q, want %q", got, want)
	}
	input = "alice@example.com at 100.101.102.103:443 via https://private.example."
	got := m.Text(input)
	for _, private := range []string{"alice", "example.com", "100.101.102.103", "private.example"} {
		if strings.Contains(got, private) {
			t.Errorf("sensitive %q remains in %q", private, got)
		}
	}
	if !strings.HasSuffix(got, ".") {
		t.Errorf("sentence punctuation lost: %q", got)
	}
}

func TestCommand(t *testing.T) {
	m := New()
	for _, tt := range []struct{ input, want []string }{
		{nil, nil},
		{[]string{}, []string{}},
		{[]string{"/home/alice/bin/tool"}, []string{"tool"}},
		{[]string{`C:\Users\alice\bin\tool.exe`, "secret"}, []string{"tool.exe", "[arguments withheld]"}},
		{[]string{"tool", "--token", "secret", "alice@example.com"}, []string{"tool", "[arguments withheld]"}},
		{[]string{""}, []string{""}},
	} {
		before := append([]string{}, tt.input...)
		if got := m.Command(tt.input); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Command(%v) = %v, want %v", tt.input, got, tt.want)
		}
		if len(tt.input) > 0 && !reflect.DeepEqual(tt.input, before) {
			t.Fatal("Command mutated input")
		}
	}
}

func TestEnabledFromEnv(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "YeS", "on", "ON", " true "} {
		t.Setenv("MESH_PRIVACY", value)
		if !EnabledFromEnv() {
			t.Errorf("%q did not enable masking", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "off", "2", "enabled", "tru"} {
		t.Setenv("MESH_PRIVACY", value)
		if EnabledFromEnv() {
			t.Errorf("%q enabled masking", value)
		}
	}
	m := New()
	t.Setenv("MESH_PRIVACY", "false")
	if m.Value("host", "private") == "private" {
		t.Fatal("environment changed an existing mask")
	}
}
