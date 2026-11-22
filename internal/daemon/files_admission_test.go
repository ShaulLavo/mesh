package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/update"
)

func fileFrames(t *testing.T, root string) map[string]protocol.Frame {
	t.Helper()
	target := protocol.FileTarget{HostID: fileTestHost}
	digest := digestHex([]byte("x"))
	documents := map[string]any{
		protocol.TypeFileRead:   protocol.FileReadRequest{Target: target, Path: filepath.Join(root, "a")},
		protocol.TypeFileList:   protocol.FileListRequest{Target: target, Path: root},
		protocol.TypeFileSearch: protocol.FileSearchRequest{Target: target, Path: root, Pattern: "x"},
		protocol.TypeFileWrite:  protocol.FileWriteRequest{Target: target, Path: filepath.Join(root, "created"), Data: "x", CreateOnly: true},
		protocol.TypeFilePatch:  protocol.FilePatchRequest{Target: target, Path: filepath.Join(root, "a"), ExpectedSHA256: digest, Edits: []protocol.FileEdit{{OldText: "x", NewText: "y"}}},
	}
	frames := make(map[string]protocol.Frame, len(documents))
	for requestType, document := range documents {
		raw, err := protocol.EncodeFileDocument(document)
		if err != nil {
			t.Fatal(err)
		}
		frames[requestType] = serverControlFrame(t, protocol.Control{Type: requestType, RequestID: "denied", Files: raw})
	}
	return frames
}

// A Scoped grant reads metadata and bounded session output. A real file
// request, with a valid target and a path that exists, must stop at admission
// and never reach the file owner.
func TestScopedGrantsAreDeniedEveryFileControl(t *testing.T) {
	for _, loose := range []bool{false, true} {
		f := newScopedFixture(t.Context(), t, loose)
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.server.files = &f.owners
		for requestType, frame := range fileFrames(t, root) {
			t.Run(requestType, func(t *testing.T) { assertScopedDenied(t, f, frame) })
		}
		if f.owners.calls.Load() != 0 {
			t.Fatal("a file request from a scoped grant reached the file owner")
		}
		if _, err := os.Stat(filepath.Join(root, "created")); err == nil {
			t.Fatal("a scoped grant created a file")
		}
	}
}

// The update carrier role admits only update.control frames.
func TestUpdateCarrierIsAdmittedNoFileControl(t *testing.T) {
	state := t.TempDir()
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	actor, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(t.Context(), state, actor.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	grant := auth.Bind(actor.ID)
	if grant.Admit == nil {
		t.Fatal("the update carrier has no frame admission")
	}
	if !grant.Admit(serverControlFrame(t, protocol.Control{Type: update.ControlType, RequestID: "ok"})) {
		t.Fatal("the update carrier lost its own control")
	}
	for requestType, frame := range fileFrames(t, t.TempDir()) {
		if grant.Admit(frame) {
			t.Fatalf("the update carrier was admitted %s", requestType)
		}
	}
}
