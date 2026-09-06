package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
)

func TestCanonicalGolden(t *testing.T) {
	m := Mutation{Action: Create, TargetID: KeyID(bytes.Repeat([]byte{'a'}, 32)), ClaimantID: KeyID(bytes.Repeat([]byte{'b'}, 32)), Sequence: 0x0102030405060708, PublicName: "blog.shaulavo.dev"}
	canonical, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/create.hex")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(canonical) != strings.TrimSpace(string(golden)) {
		t.Fatalf("transcript changed: %x", canonical)
	}
	digest := sha256.Sum256(canonical)
	golden, err = os.ReadFile("testdata/create.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != strings.TrimSpace(string(golden)) {
		t.Fatal("transcript digest changed")
	}
}

func TestMutationBindsEveryDomainField(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	edgeID := KeyID(bytes.Repeat([]byte{2}, ed25519.PublicKeySize))
	m, err := Sign(key, edgeID, Create, "blog.shaulavo.dev", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(m, edgeID); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Mutation){
		"action":    func(m *Mutation) { m.Action = Release },
		"edge":      func(m *Mutation) { m.TargetID = KeyID(bytes.Repeat([]byte{3}, 32)) },
		"owner":     func(m *Mutation) { m.ClaimantID = KeyID(bytes.Repeat([]byte{4}, 32)) },
		"sequence":  func(m *Mutation) { m.Sequence++ },
		"hostname":  func(m *Mutation) { m.PublicName = "other.shaulavo.dev" },
		"signature": func(m *Mutation) { m.Signature = append([]byte(nil), m.Signature...); m.Signature[0] ^= 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := m
			mutate(&changed)
			if _, err := Verify(changed, edgeID); err == nil {
				t.Fatal("accepted modified signed mutation")
			}
		})
	}
}

func TestMutationRejectsInvalidDomain(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	id := KeyID(key.Public().(ed25519.PublicKey))
	for _, name := range []string{"", "blog", "*.shaulavo.dev", "localhost", "127.0.0.1", "::1", "shaulavo.dev", "two.labels.shaulavo.dev", "Blog.shaulavo.dev", "blog.shaulavo.dev.", strings.Repeat("a", 4096)} {
		if _, err := Sign(key, id, Create, name, 1); err == nil {
			t.Fatalf("accepted hostname %q", name)
		}
	}
	for _, sequence := range []uint64{0, math.MaxInt64 + 1, math.MaxUint64} {
		if _, err := Sign(key, id, Create, "blog.shaulavo.dev", sequence); err == nil {
			t.Fatalf("accepted sequence %d", sequence)
		}
	}
	if _, err := Sign(key, id, Create, "blog.shaulavo.dev", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(nil, id, Create, "blog.shaulavo.dev", 1); err == nil {
		t.Fatal("accepted missing private key")
	}
}
