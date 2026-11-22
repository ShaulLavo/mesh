package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func requireNoStagedFiles(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".mesh-fs-") {
				t.Fatalf("temporary file %s survived in %s", entry.Name(), dir)
			}
		}
	}
}

func (f *fileFixture) mkdir(name string) {
	f.t.Helper()
	if err := os.Mkdir(f.path(name), 0o750); err != nil {
		f.t.Fatal(err)
	}
}

// Replacing a directory with a symlink while a search is under way cannot send
// the search outside its tree.
func TestFileSearchDoesNotFollowADirectoryReplacedDuringTraversal(t *testing.T) {
	f := newFileFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("Alpha secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		f.mkdir(name)
		f.put(name+"/hit.txt", []byte("Alpha in "+name+"\n"))
	}
	swapped := false
	f.controller.hook = func(stage, path string) {
		if stage != "enter-directory" || path != f.path("b") {
			return
		}
		swapped = true
		if err := os.RemoveAll(f.path("b")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, f.path("b")); err != nil {
			t.Error(err)
		}
	}
	result := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "Alpha"})
	if !swapped {
		t.Fatal("the test never replaced the directory")
	}
	for _, match := range result.Matches {
		if strings.Contains(match.Text, "secret") {
			t.Fatalf("the search followed a replaced directory out of its tree: %+v", match)
		}
	}
	if len(result.Matches) != 1 || result.Matches[0].Text != "Alpha in a" {
		t.Fatalf("matches: %+v", result.Matches)
	}
}

// A directory already open when it is renamed away keeps listing what it held.
func TestFileSearchStaysInTheDirectoryItOpened(t *testing.T) {
	f := newFileFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hit.txt"), []byte("Alpha outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mkdir("inner")
	f.put("inner/hit.txt", []byte("Alpha inside\n"))
	f.controller.hook = func(stage, path string) {
		if stage != "listed-directory" || path != f.path("inner") {
			return
		}
		if err := os.Rename(f.path("inner"), f.path("inner.moved")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, f.path("inner")); err != nil {
			t.Error(err)
		}
	}
	result := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "Alpha"})
	if len(result.Matches) != 1 || result.Matches[0].Text != "Alpha inside" {
		t.Fatalf("matches: %+v", result.Matches)
	}
}

// Staging, the install and cleanup all go through the directory that was
// opened, wherever its path points afterwards.
func TestFileWriteStagesInTheDirectoryItOpened(t *testing.T) {
	for _, createOnly := range []bool{false, true} {
		f := newFileFixture(t)
		outside := t.TempDir()
		f.mkdir("d")
		f.put("d/x.txt", []byte("old"))
		f.controller.hook = func(stage, _ string) {
			if stage != "before-commit" {
				return
			}
			if err := os.Rename(f.path("d"), f.path("d.moved")); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, f.path("d")); err != nil {
				t.Error(err)
			}
		}
		request := protocol.FileWriteRequest{Path: f.path("d/x.txt"), Data: "new", ExpectedSHA256: digestHex([]byte("old"))}
		moved := "d.moved/x.txt"
		if createOnly {
			request = protocol.FileWriteRequest{Path: f.path("d/y.txt"), Data: "new", CreateOnly: true}
			moved = "d.moved/y.txt"
		}
		fileOK[protocol.FileWriteResult](t, f.write(request), protocol.TypeFileWriteResult)
		f.requireContent(f.path(moved), []byte("new"))
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("the write reached the directory that replaced the parent: %v", entries)
		}
		requireNoStagedFiles(t, f.path("d.moved"), outside)
	}
}

func TestFileWriteFailureCleansTheDirectoryItStagedIn(t *testing.T) {
	f := newFileFixture(t)
	outside := t.TempDir()
	f.mkdir("d")
	f.put("d/x.txt", []byte("old"))
	f.controller.hook = func(stage, _ string) {
		if stage != "before-commit" {
			return
		}
		if err := os.Rename(f.path("d"), f.path("d.moved")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, f.path("d")); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(f.path("d.moved/x.txt"), []byte("saved by an editor"), 0o600); err != nil {
			t.Error(err)
		}
	}
	fileCode(t, f.write(protocol.FileWriteRequest{Path: f.path("d/x.txt"), Data: "new", ExpectedSHA256: digestHex([]byte("old"))}), "file.conflict")
	f.requireContent(f.path("d.moved/x.txt"), []byte("saved by an editor"))
	requireNoStagedFiles(t, f.path("d.moved"), outside)
}

// A save that lands after the digest check, in the gap before the install, is
// found by the swap and put back.
func TestFileWriteDoesNotOverwriteASaveInTheCommitGap(t *testing.T) {
	cases := map[string]func(f *fileFixture, path string){
		"in place": func(f *fileFixture, path string) {
			if err := os.WriteFile(path, []byte("saved in place by an editor"), 0o600); err != nil {
				f.t.Error(err)
			}
		},
		"by rename": func(f *fileFixture, path string) {
			if err := os.Rename(f.put("editor.tmp", []byte("saved by rename")), path); err != nil {
				f.t.Error(err)
			}
		},
	}
	for name, save := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFileFixture(t)
			path := f.put("doc.txt", []byte("old"))
			f.backdate(path)
			f.controller.hook = func(stage, _ string) {
				if stage == "installing" {
					save(f, path)
				}
			}
			fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "new", ExpectedSHA256: digestHex([]byte("old"))}), "file.conflict")
			got, err := os.ReadFile(path) //nolint:gosec // the test names its own temporary file
			if err != nil || strings.Contains(string(got), "new") || string(got) == "old" {
				t.Fatalf("the editor's save was lost: %q, %v", got, err)
			}
			requireNoStagedFiles(t, f.root)
		})
	}
}

// When a third writer replaces the file after the swap, the version the swap
// displaced is kept under a named file instead of deleted.
func TestFileWriteKeepsADisplacedVersionItCannotRestore(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("doc.txt", []byte("old"))
	f.backdate(path)
	f.controller.hook = func(stage, _ string) {
		switch stage {
		case "installing":
			if err := os.WriteFile(path, []byte("first editor save"), 0o600); err != nil {
				t.Error(err)
			}
		case "exchanged":
			if err := os.Rename(f.put("second.tmp", []byte("second editor save")), path); err != nil {
				t.Error(err)
			}
		}
	}
	response := f.write(protocol.FileWriteRequest{Path: path, Data: "new", ExpectedSHA256: digestHex([]byte("old"))})
	fileCode(t, response, "file.conflict")
	f.requireContent(path, []byte("second editor save"))
	var kept string
	entries, _ := os.ReadDir(f.root)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mesh-fs-") {
			kept = entry.Name()
		}
	}
	if kept == "" || !strings.Contains(response.Message, kept) {
		t.Fatalf("the displaced version is not reported: kept %q, message %q", kept, response.Message)
	}
	f.requireContent(f.path(kept), []byte("first editor save"))
}

// The result describes the file this write installed, not what a later
// writer left at the path.
func TestFileWriteReportsTheRevisionItInstalled(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("doc.txt", []byte("old"))
	result := fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: path, Data: "new", ExpectedSHA256: digestHex([]byte("old"))}), protocol.TypeFileWriteResult)
	if got := f.readPage(protocol.FileReadRequest{Path: path}); got.Revision != result.Revision {
		t.Fatalf("revision %s, a read of the installed file says %s", result.Revision, got.Revision)
	}
	f.controller.hook = func(stage, _ string) {
		if stage == "committed" {
			if err := os.Rename(f.put("later.tmp", []byte("a later save")), path); err != nil {
				t.Error(err)
			}
		}
	}
	later := fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: path, Data: "newer", ExpectedSHA256: result.SHA256}), protocol.TypeFileWriteResult)
	if now := f.readPage(protocol.FileReadRequest{Path: path}); now.Revision == later.Revision || later.SHA256 != digestHex([]byte("newer")) {
		t.Fatal("the result reports the next writer's file")
	}
}

// The request and the grant are checked once more immediately before the
// install, after any wait for the commit slot.
func TestFileWriteChecksTheRequestAndGrantBeforeCommit(t *testing.T) {
	for _, createOnly := range []bool{false, true} {
		for _, how := range []string{"cancelled", "revoked"} {
			f := newFileFixture(t)
			path := f.put("doc.txt", []byte("old"))
			request := protocol.FileWriteRequest{Path: path, Data: "new", ExpectedSHA256: digestHex([]byte("old"))}
			if createOnly {
				path = f.path("fresh.txt")
				request = protocol.FileWriteRequest{Path: path, Data: "new", CreateOnly: true}
			}
			ctx, cancel := context.WithCancel(localServerContext(t.Context()))
			if how == "cancelled" {
				f.controller.hook = func(stage, _ string) {
					if stage == "before-commit" {
						cancel()
					}
				}
			} else {
				f.controller.admit = func(context.Context) error { return access.ErrDenied }
			}
			request.Target = f.target()
			raw, _ := protocol.EncodeFileDocument(request)
			response, _, err := f.controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeFileWrite, RequestID: "w", Files: raw})
			cancel()
			if err != nil || response.Type != protocol.TypeError {
				t.Fatalf("%s: %+v %v", how, response, err)
			}
			if createOnly {
				if _, err := os.Stat(path); err == nil {
					t.Fatalf("%s: a refused create wrote the file", how)
				}
			} else {
				f.requireContent(path, []byte("old"))
			}
			requireNoStagedFiles(t, f.root)
		}
	}
}

// A write waiting for the commit slot ends when its caller disconnects, and
// does not write afterwards.
func TestFileWriteWaitingForTheCommitSlotStopsWhenTheCallerLeaves(t *testing.T) {
	f := newFileFixture(t)
	path := f.path("never.txt")
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, &contextWorkerCounter{})
	server, err := newClientServer(lifecycle, &contextWorkerCounter{}, &contextControlCounter{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.files = f.controller
	f.controller.writeSlot <- struct{}{} // Another commit holds the slot.
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(localServerContext(t.Context()), client) }()
	raw, err := protocol.EncodeFileDocument(protocol.FileWriteRequest{Target: f.target(), Path: path, Data: "late", CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeFileWrite, RequestID: "waiting", Files: raw}))
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "file server"); err != nil {
		t.Fatal(err)
	}
	<-f.controller.writeSlot
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a write whose caller left was committed")
	}
	requireNoStagedFiles(t, f.root)
}

// A device revoked while its reply waits in the relay queue receives nothing.
func TestQueuedFileReplyUsesLatchedFullGrant(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
	controller := newFileController(host.ID)
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := newRelayTestConn()
	relay := newClientRelay(conn, nil, defaultWorkerOperationTimeout)
	t.Cleanup(func() { _ = relay.Close() })
	entered, release := conn.blockWrites()
	defer release()
	prior := protocol.Frame{Kind: protocol.KindControl, Payload: []byte("previous reply")}
	if err := relay.enqueueResponse(prior); err != nil {
		t.Fatal(err)
	}
	waitRelaySignal(t, entered, "first reply held at writer")
	raw, err := protocol.EncodeFileDocument(protocol.FileReadRequest{Target: protocol.FileTarget{HostID: host.ID}, Path: file})
	if err != nil {
		t.Fatal(err)
	}
	server := &clientServer{files: controller}
	if err := server.replyFiles(grantCtx, relay, newFrameLookahead(conn), protocol.Control{Type: protocol.TypeFileRead, RequestID: "queued", Files: raw}); err != nil {
		t.Fatal(err)
	}
	if err := identity.RevokeDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	release()
	conn.waitClosed(t)
	if sent := conn.nextWrite(t); string(sent.Payload) != string(prior.Payload) {
		t.Fatalf("unexpected first write: %q", sent.Payload)
	}
	select {
	case leaked := <-conn.writes:
		t.Fatalf("revoked grant received a queued file reply: %s", leaked.Payload)
	default:
	}
}

func TestFileSearchStopsAtTheByteBudgetThroughTheSearchPath(t *testing.T) {
	f := newFileFixture(t)
	f.controller.searchBytes = 100
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		f.put(name, []byte(strings.Repeat("hit ", 15)+"\n"))
	}
	result := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "hit"})
	if result.Truncated != protocol.FileTruncatedBytes || len(result.Matches) != 1 {
		t.Fatalf("truncated %q with %d matches", result.Truncated, len(result.Matches))
	}
}

func TestFileReadCursorNeedsARevision(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", patternBytes(4096))
	bare := f.controller.encodeCursor(fileCursor{Kind: fileCursorRead, Offset: 10}, path)
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: bare}), "file.cursor_invalid")
}
