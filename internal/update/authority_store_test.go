package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func mustRandomID(t *testing.T) string {
	t.Helper()
	id, err := newRandomID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuthorityRejectsUnsafeAndDamagedState(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "permissions", "duplicate", "unknown", "oversize"} {
		t.Run(kind, func(t *testing.T) { testAuthorityUnsafe(t, kind) })
	}
}
func testAuthorityUnsafe(t *testing.T, kind string) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetHostPolicy(t.Context(), 0, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(store.directory))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	path := "host-policy.json"
	data, err := root.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "symlink":
		original := path + ".original"
		if err := root.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink(original, path); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := root.Link(path, path+".link"); err != nil {
			t.Fatal(err)
		}
	case "permissions":
		if err := root.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	case "duplicate":
		writeAuthorityCorrupt(t, root, path, append([]byte(`{"schema":1,`), data[1:]...))
	case "unknown":
		writeAuthorityCorrupt(t, root, path, append([]byte(`{"owner":"claimed",`), data[1:]...))
	case "oversize":
		writeAuthorityCorrupt(t, root, path, make([]byte, maximumMessage+1))
	}
	if policy, err := store.HostPolicy(t.Context()); err == nil {
		t.Fatalf("damaged authority read as a policy: %+v", policy)
	}
	if _, err := root.Lstat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("damaged authority was deleted")
	}
}
func writeAuthorityCorrupt(t *testing.T, root *os.Root, path string, data []byte) {
	t.Helper()
	if err := root.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityPreservesLegacyRelaysAndBindsConfigurationActions(t *testing.T) {
	serverID, serverKey := testIdentity(t)
	actorID, actorKey := testIdentity(t)
	relayID, relayKey := testIdentity(t)
	state := t.TempDir()
	if err := Trust(t.Context(), state, actorID, false); err != nil {
		t.Fatal(err)
	}
	authority := &Authority{StateDir: state, ID: serverID, Key: serverKey, Handle: func(_ context.Context, actor VerifiedActor, _ string, _ json.RawMessage) (any, error) {
		return actor.ID(), nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: &transport.Authentication{Key: serverKey, Authorize: func(id string) bool { return id == relayID }}}, func(ctx context.Context, conn transport.Conn) error {
			return serveRelayedControls(ctx, conn, authority)
		})
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, server.URL, transport.DialOptions{Auth: &transport.Authentication{Key: relayKey, ExpectedIdentity: serverID}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := Client{ID: actorID, Key: actorKey}
	host := Host{ID: serverID, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}
	for _, action := range []string{"info", "configuration.status", "configuration.observe"} {
		request := Message{Actor: actorID, Target: serverID, Action: "challenge", Nonce: mustRandomID(t)}
		challenge, err := client.exchange(conn, host, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(challenge.Data, &request.Nonce); err != nil {
			t.Fatal(err)
		}
		request.Action = action
		response, err := client.exchange(conn, host, request)
		if action == "info" {
			assertRelaySigner(t, response, err, actorID)
			continue
		}
		var denied *RemoteError
		if !errors.As(err, &denied) || denied.Code != "configuration.denied" {
			t.Fatalf("new %s boundary accepted relay: %v", action, err)
		}
	}
}

func TestUpdateResponseReplacesWholeSnapshot(t *testing.T) {
	old := ConfigurationStatus{Revision: 2, Change: &ConfigurationChange{Blocked: "old block", Targets: []ConfigurationTarget{{HostID: "recipient", Problem: "old failure"}}}}
	if err := decodeWholeOutput([]byte(`{"revision":3}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Revision != 3 || old.Change != nil {
		t.Fatalf("omitted fields retained stale status: %+v", old)
	}
}

func serveRelayedControls(ctx context.Context, conn transport.Conn, authority *Authority) error {
	for range 6 {
		if err := exchangeDownloadPlan(ctx, conn, authority); err != nil {
			return err
		}
	}
	return nil
}
func assertRelaySigner(t *testing.T, response Message, err error, expected string) {
	t.Helper()
	var id string
	if err != nil || json.Unmarshal(response.Data, &id) != nil || id != expected {
		t.Fatalf("legacy signer lost: %s %v", id, err)
	}
}
