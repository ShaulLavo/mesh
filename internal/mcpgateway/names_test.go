package mcpgateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testHostID(t *testing.T, seed byte) string {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func testServiceID(seed byte) string {
	raw := make([]byte, 16)
	for i := range raw {
		raw[i] = seed ^ byte(i*7)
	}
	return hex.EncodeToString(raw)
}

func TestEncodedNamesRoundTripAndNeverCollide(t *testing.T) {
	host := testHostID(t, 1)
	service := testServiceID(9)
	seen := map[string]Identity{}
	// Pairs that a naive separator scheme would merge: underscores, case,
	// prefixes of each other and non-ASCII text.
	names := []string{"a_b", "a", "b", "a_", "_a", "A_b", "a__b", "über", "Über", "x.y", "x-y", "日本"}
	for _, kind := range []string{KindTool, KindPrompt} {
		for _, name := range names {
			id := Identity{HostID: host, ServiceID: service, Kind: kind, Name: name}
			exposed, fits, err := EncodedName(id)
			if err != nil || !fits {
				t.Fatalf("%v: %q fits=%v err=%v", id, exposed, fits, err)
			}
			if prev, dup := seen[exposed]; dup {
				t.Fatalf("%v and %v both expose %q", prev, id, exposed)
			}
			seen[exposed] = id
			back, err := DecodeName(exposed)
			if err != nil || back != id {
				t.Fatalf("decode %q = %v, %v; want %v", exposed, back, err, id)
			}
			if strings.ContainsAny(exposed, "ABCDEFGHIJKLMNOPQRSTUVWXYZ=") {
				t.Fatalf("%q is not lowercase unpadded", exposed)
			}
		}
	}
	other, _, _ := EncodedName(Identity{HostID: testHostID(t, 2), ServiceID: service, Kind: KindTool, Name: "a"})
	if _, dup := seen[other]; dup {
		t.Fatal("another host exposes the same name")
	}
}

func TestDecodeNameRejectsNonCanonicalForms(t *testing.T) {
	exposed, _, err := EncodedName(Identity{HostID: testHostID(t, 3), ServiceID: testServiceID(1), Kind: KindTool, Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		strings.ToUpper(exposed),
		strings.Replace(exposed, "_t_", "_x_", 1),
		exposed + "_",
		"h_" + strings.TrimPrefix(exposed, "h_")[1:],
		"mesh_exec",
		"hx_1",
	}
	for _, name := range bad {
		if _, err := DecodeName(name); !errors.Is(err, ErrUnknownName) {
			t.Fatalf("%q decoded: %v", name, err)
		}
	}
}

func TestOverlongNamesGetStableOverflowAllocations(t *testing.T) {
	store := NewStore(t.TempDir())
	long := Identity{HostID: testHostID(t, 4), ServiceID: testServiceID(2), Kind: KindTool, Name: strings.Repeat("n", 80)}
	if _, fits, _ := EncodedName(long); fits {
		t.Fatal("80-byte name should overflow 128 characters")
	}
	first, err := store.ExposedName(context.Background(), long)
	if err != nil || first != "hx_1" {
		t.Fatalf("first allocation %q, %v", first, err)
	}
	again, err := store.ExposedName(context.Background(), long)
	if err != nil || again != first {
		t.Fatalf("same identity reallocated: %q, %v", again, err)
	}
	other := long
	other.Kind = KindPrompt
	second, _ := store.ExposedName(context.Background(), other)
	if second != "hx_2" {
		t.Fatalf("second allocation %q", second)
	}
	file, _ := store.Load()
	if id, err := ResolveName(file, "hx_1"); err != nil || id != long {
		t.Fatalf("resolve hx_1 = %v, %v", id, err)
	}
	if _, err := ResolveName(file, "hx_3"); !errors.Is(err, ErrUnknownName) {
		t.Fatalf("absent allocation resolved: %v", err)
	}
	// Removing the mapping by hand must never let the counter hand the
	// name to a new identity.
	_, err = store.Update(context.Background(), nil, func(f *File) error {
		delete(f.Overflow.Names, "hx_1")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement := long
	replacement.Name = strings.Repeat("m", 80)
	if name, _ := store.ExposedName(context.Background(), replacement); name != "hx_3" {
		t.Fatalf("allocation reused: %q", name)
	}
}

func TestResourceURIsKeepTheirRawEncoding(t *testing.T) {
	host, service := testHostID(t, 5), testServiceID(3)
	originals := []string{
		"file:///work/a%20b.txt",
		"repo://x/y?q=a%2Fb#frag",
		"mesh-mcp://nested/uri",
		"urn:ietf:rfc:2648",
		"https://example.test/日本",
	}
	for _, original := range originals {
		uri, err := EncodeURI(host, service, original)
		if err != nil {
			t.Fatal(err)
		}
		h, s, back, err := DecodeURI(uri, false)
		if err != nil || h != host || s != service || back != original {
			t.Fatalf("%q -> %q -> %q %q %q %v", original, uri, h, s, back, err)
		}
	}
	template, _ := EncodeURI(host, service, "repo://{owner}/{name}/file{?ref}")
	if _, _, back, err := DecodeURI(template, true); err != nil || back != "repo://{owner}/{name}/file{?ref}" {
		t.Fatalf("template %q %v", back, err)
	}
	for _, bad := range []string{"mesh-mcp://", "file:///x", "mesh-mcp://AAAA/bbbb/x", "mesh-mcp://" + strings.Repeat("a", 52) + "/x/y"} {
		if _, _, _, err := DecodeURI(bad, false); err == nil {
			t.Fatalf("%q decoded", bad)
		}
	}
	relative, _ := EncodeURI(host, service, "not a uri")
	if _, _, _, err := DecodeURI(relative, false); err == nil {
		t.Fatal("a wrapped relative reference decoded as a resource URI")
	}
}

func TestStoreCompareAndSwapAndPrivacy(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	zero := uint64(0)
	sel := Selection{Alias: "repo", HostID: testHostID(t, 6), ServiceID: testServiceID(4), Include: []string{"*"}, Exclude: []string{}}
	f, err := store.Update(context.Background(), &zero, func(f *File) error {
		f.Profiles["dev"] = Profile{Services: []Selection{sel}}
		return nil
	})
	if err != nil || f.Revision != 1 {
		t.Fatalf("first write %d %v", f.Revision, err)
	}
	if _, err := store.Update(context.Background(), &zero, func(*File) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	same, err := store.Update(context.Background(), nil, func(*File) error { return nil })
	if err != nil || same.Revision != 1 {
		t.Fatalf("no-op change moved the revision: %d %v", same.Revision, err)
	}
	info, err := os.Stat(filepath.Join(dir, ProfilesFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("profiles file mode %v %v", info.Mode(), err)
	}
	_, err = store.Update(context.Background(), nil, func(f *File) error {
		p := f.Profiles["dev"]
		p.Services = append(p.Services, Selection{Alias: "repo", HostID: sel.HostID, ServiceID: testServiceID(5)})
		f.Profiles["dev"] = p
		return nil
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate alias accepted: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, ProfilesFileName), 0o644); err != nil { //nolint:gosec // The test makes the file readable on purpose.
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("a readable profiles file was trusted")
	}
}

func TestSelectionFilters(t *testing.T) {
	sel := Selection{Include: []string{"git_*", "echo"}, Exclude: []string{"git_push"}}
	for name, want := range map[string]bool{"git_log": true, "git_push": false, "echo": true, "rm": false} {
		if sel.Allows(name) != want {
			t.Fatalf("%s: want %v", name, want)
		}
	}
	if !(Selection{}).Allows("anything") {
		t.Fatal("no include list should allow every name")
	}
}

func TestCursorSignerBindsIdentity(t *testing.T) {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	a, b := newCursorSigner(secret), newCursorSigner(secret)
	c := pageCursor{Version: 1, Profile: "dev", Revision: 7, Method: "tools/list", Snapshot: "x", Offset: 200}
	token := a.sign(c)
	if got, err := b.verify(token); err != nil || got != c {
		t.Fatalf("a fresh frontend with the same identity rejected the cursor: %v", err)
	}
	other := newCursorSigner([]byte("another identity"))
	if _, err := other.verify(token); err == nil {
		t.Fatal("another identity accepted the cursor")
	}
	if _, err := a.verify(token[:len(token)-2] + "AA"); err == nil {
		t.Fatal("a tampered cursor verified")
	}
}
