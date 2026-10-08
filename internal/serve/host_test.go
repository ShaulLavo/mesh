package serve

import "testing"

func TestCanonicalHost(t *testing.T) {
	for _, test := range []struct {
		authority string
		want      string
	}{
		{authority: "PC.EXAMPLE.TS.NET.:443", want: "pc.example.ts.net"},
		{authority: "APP.MESH.TEST.:12000", want: "app.mesh.test"},
		{authority: "LOCALHOST.", want: "localhost"},
		{authority: "127.99.12.34:12000", want: "127.99.12.34"},
		{authority: "[::1]:12000", want: "::1"},
		{authority: "[::1]", want: "::1"},
		{authority: "fd7a:115c:a1e0::1", want: "fd7a:115c:a1e0::1"},
		{authority: "[::ffff:127.0.0.1]:12000", want: "::ffff:127.0.0.1"},
	} {
		t.Run(test.authority, func(t *testing.T) {
			if got, ok := CanonicalHost(test.authority); !ok || got != test.want {
				t.Fatalf("CanonicalHost(%q) = %q, %t; want %q, true", test.authority, got, ok, test.want)
			}
		})
	}
	for _, authority := range []string{
		"", "localhost:", "localhost:bad", "localhost:0", "localhost:65536",
		"[localhost]:443", "[127.0.0.1]:443", "[::1%lo]:443", "app.mesh.test..",
		"app.mesh.test/path", "attacker@app.mesh.test", "app.mesh.test ",
	} {
		t.Run(authority, func(t *testing.T) {
			if got, ok := CanonicalHost(authority); ok {
				t.Fatalf("malformed authority %q accepted as %q", authority, got)
			}
		})
	}
}
