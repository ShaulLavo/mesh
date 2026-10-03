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
	alias := m.Value("account", "personal-handle")
	if !regexp.MustCompile(`^account-[0-9a-f]{6}$`).MatchString(alias) {
		t.Fatalf("unexpected alias %q", alias)
	}
	for _, mask := range []*Mask{m, New(), {}} {
		if got := mask.Value("account", "personal-handle"); got != alias {
			t.Fatalf("alias changed: %q != %q", got, alias)
		}
	}
	if got := m.Value("host", ""); got != "" {
		t.Fatalf("empty value = %q", got)
	}
	if got := m.Value("owner", "personal-handle"); got == alias {
		t.Fatal("different kinds share an alias")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got := m.Value("account", "personal-handle"); got != alias {
				t.Errorf("concurrent alias = %q, want %q", got, alias)
			}
		})
	}
	wg.Wait()
}

func TestReadableKindsAndOpaqueCategories(t *testing.T) {
	m := New()
	for _, kind := range []string{"host", "session", "title", "service", "name", "app", "label", "route", "fleet", "notice"} {
		if got := m.Value(kind, "mesh dev-server"); got != "mesh dev-server" {
			t.Errorf("readable %s = %q", kind, got)
		}
		value := "dev on alice@machine at /home/alice/project"
		want := "dev on " + m.Value("username", "alice") + "@machine at ~/project"
		if got := m.Value(kind, value); got != want {
			t.Errorf("embedded sensitivity for %s = %q, want %q", kind, got, want)
		}
	}
	for _, kind := range []string{"account", "owner", "email", "user", "username", "handle", "error", "context", "conversation", "update", "host-id", "revision", "upload"} {
		for _, value := range []string{"alice", "alice@example.com", "token=unknown-secret"} {
			got := m.Value(kind, value)
			if !regexp.MustCompile(`^` + regexp.QuoteMeta(kind) + `-[0-9a-f]{6}$`).MatchString(got) {
				t.Errorf("opaque %s = %q", kind, got)
			}
			if got != New().Value(kind, value) {
				t.Errorf("unstable %s alias", kind)
			}
		}
	}
}

func TestReadablePaths(t *testing.T) {
	m := New()
	for _, tt := range []struct{ input, want string }{
		{"/home/alice/project", "~/project"},
		{"/Users/alice/project", "~/project"},
		{"/home/alice", "~"},
		{"/Users/alice/", "~/"},
		{"/home/", "/home/"},
		{"/Users/", "/Users/"},
		{"/homework/alice", "/homework/alice"},
		{"/work/projects/mesh", "/work/projects/mesh"},
		{"relative/project", "relative/project"},
		{"~/project", "~/project"},
		{"", ""},
	} {
		if got := m.Value("path", tt.input); got != tt.want {
			t.Errorf("path %q = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestReadableURLs(t *testing.T) {
	m := New()
	for _, tt := range []struct{ input, want string }{
		{"https://private.example:8443/app?token=secret#fragment", "https://<domain>:8443/app"},
		{"https://alice:password@machine.tail1234.ts.net:8443/app?q=secret#fragment", "https://machine.<tailnet>:8443/app"},
		{"http://localhost:3000/app", "http://<domain>:3000/app"},
		{"http://100.101.102.103:8080/app", "http://" + m.Value("ip", "100.101.102.103") + ":8080/app"},
		{"http://[2001:db8::1]:8080/app", "http://" + m.Value("ip", "2001:db8::1") + ":8080/app"},
		{"https://private.example/with%20space", "https://<domain>/with%20space"},
	} {
		if got := m.Value("url", tt.input); got != tt.want {
			t.Errorf("url %q = %q, want %q", tt.input, got, tt.want)
		}
		if got := m.Text(tt.input); got != tt.want {
			t.Errorf("text url %q = %q, want %q", tt.input, got, tt.want)
		}
	}
	for _, input := range []string{"not a url", "https://bad%host/path", "file:///home/alice/project"} {
		if got := m.Value("url", input); !regexp.MustCompile(`^url-[0-9a-f]{6}$`).MatchString(got) {
			t.Errorf("invalid or authority-free URL not withheld: %q", got)
		}
	}
}

func TestIdentifiersInsidePathsAndRoutes(t *testing.T) {
	m := New()
	const uuid = "550e8400-e29b-41d4-a716-446655440000"
	const ipv4 = "100.101.102.103"
	const ipv6 = "2001:db8::1"
	for _, tt := range []struct{ kind, input, want string }{
		{"path", "/home/alice/claude/" + uuid, "~/claude/" + m.Value("uuid", uuid)},
		{"path", "/Users/alice/claude/" + uuid, "~/claude/" + m.Value("uuid", uuid)},
		{"path", "/work/cache/" + ipv4 + "/" + ipv6, "/work/cache/" + m.Value("ip", ipv4) + "/" + m.Value("ip", ipv6)},
		{"path", "/work/cache/" + uuid, "/work/cache/" + m.Value("uuid", uuid)},
		{"url", "https://private.example/claude/" + uuid, "https://<domain>/claude/" + m.Value("uuid", uuid)},
		{"url", "https://" + uuid + ".ts.net/project", "https://" + m.Value("uuid", uuid) + ".<tailnet>/project"},
		{"url", uuid + ".ts.net/project", m.Value("uuid", uuid) + ".<tailnet>/project"},
		{"url", "https://private.example/with%20space/" + uuid + "/" + ipv4, "https://<domain>/with%20space/" + m.Value("uuid", uuid) + "/" + m.Value("ip", ipv4)},
		{"route", "omarchy.mesh.shaulavo.dev/ai", "<domain>/ai"},
		{"route", "omarchy.mesh.shaulavo.dev/ai/" + uuid, "<domain>/ai/" + m.Value("uuid", uuid)},
		{"url", "omarchy.mesh.shaulavo.dev:8443/ai?token=secret#private", "<domain>:8443/ai"},
		{"url", "machine.tail1234.ts.net/ai", "machine.<tailnet>/ai"},
	} {
		if got := m.Value(tt.kind, tt.input); got != tt.want {
			t.Errorf("Value(%q, %q) = %q, want %q", tt.kind, tt.input, got, tt.want)
		}
		// Scheme-free URL fields are known URLs, whereas Text intentionally
		// recognizes only scheme-bearing URLs and known private DNS routes.
		if tt.kind != "url" || strings.Contains(tt.input, "://") {
			if got := m.Text(tt.input); got != tt.want {
				t.Errorf("Text(%q) = %q, want %q", tt.input, got, tt.want)
			}
		}
	}
	for _, input := range []string{"omarchy", "dev-server", "omarchy.<tailnet>", "<domain>/ai"} {
		if got := m.Value("route", input); got != input {
			t.Errorf("ordinary or sanitized route changed: %q => %q", input, got)
		}
	}
}

func TestAccountsInRetainedPathsAndSensitiveHosts(t *testing.T) {
	m := New()
	const email = "alice@example.com"
	const uuid = "550e8400-e29b-41d4-a716-446655440000"
	const ipv6 = "2001:db8::1"
	account := m.Value("email", email)
	user := m.Value("username", "alice")
	for _, tt := range []struct{ kind, input, want string }{
		{"path", "/work/accounts/" + email + "/project", "/work/accounts/" + account + "/project"},
		{"path", "/home/alice/accounts/" + email + "/project", "~/accounts/" + account + "/project"},
		{"path", "/work/accounts/alice@machine/project", "/work/accounts/" + user + "@machine/project"},
		{"path", "/work/accounts/alice@" + uuid + "/project", "/work/accounts/" + user + "@" + m.Value("uuid", uuid) + "/project"},
		{"url", "https://private.example/" + email + "/project", "https://<domain>/" + account + "/project"},
		{"url", "https://private.example/alice%40example.com/project", "https://<domain>/" + account + "/project"},
		{"url", "https://private.example/with%20space/alice%40" + uuid + "/project", "https://<domain>/with%20space/" + user + "@" + m.Value("uuid", uuid) + "/project"},
		{"url", "https://private.example/escaped%2Falice%40example.com/project", "https://<domain>/escaped%2F" + account + "/project"},
		{"host", "alice@" + uuid, user + "@" + m.Value("uuid", uuid)},
		{"host", "alice@" + ipv6, user + "@" + m.Value("ip", ipv6)},
		{"host", "alice@[" + ipv6 + "]", user + "@" + m.Value("ip", "["+ipv6+"]")},
	} {
		if got := m.Value(tt.kind, tt.input); got != tt.want {
			t.Errorf("Value(%q, %q) = %q, want %q", tt.kind, tt.input, got, tt.want)
		}
		if got := m.Text(tt.input); got != tt.want {
			t.Errorf("Text(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
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
	tests := []struct{ value, want string }{
		{"alice@example.com", m.Value("email", "alice@example.com")},
		{"first.last+mesh@example.co.uk", m.Value("email", "first.last+mesh@example.co.uk")},
		{"https://private.example:8443/app?token=secret#fragment", "https://<domain>:8443/app"},
		{"ssh://alice@machine/private/path", "ssh://<domain>/private/path"},
		{"/home/alice/private/project", "~/private/project"},
		{"/Users/alice/project", "~/project"},
		{"/home/alice", "~"},
		{"/work/projects/private", "/work/projects/private"},
		{"~/.ssh/id_ed25519", "~/.ssh/id_ed25519"},
		{`C:\Users\alice\private`, `C:\Users\alice\private`},
		{"machine.tail1234.ts.net", "machine.<tailnet>"},
		{"machine.TAIL1234.TS.NET", "machine.<tailnet>"},
		{"machine.tailscale.net", "machine.<tailnet>"},
		{"550e8400-e29b-41d4-a716-446655440000", m.Value("uuid", "550e8400-e29b-41d4-a716-446655440000")},
		{"alice@machine", m.Value("username", "alice") + "@machine"},
		{"alice@machine.tail1234.ts.net", m.Value("username", "alice") + "@machine.<tailnet>"},
		{"100.101.102.103", m.Value("ip", "100.101.102.103")},
		{"127.0.0.1", m.Value("ip", "127.0.0.1")},
		{"2001:db8::1", m.Value("ip", "2001:db8::1")},
		{"::1", m.Value("ip", "::1")},
		{"fe80::1234%eth0", m.Value("ip", "fe80::1234%eth0")},
		{"[2001:db8::1]", m.Value("ip", "[2001:db8::1]")},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			input := "failed (" + tt.value + "), retry"
			want := "failed (" + tt.want + "), retry"
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
	if m.Value("account", "private") == "private" {
		t.Fatal("environment changed an existing mask")
	}
}
