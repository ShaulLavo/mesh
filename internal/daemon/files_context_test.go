package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

// contextFileFixture serves the file controls on the same host and journal as
// a real context store.
type contextFileFixture struct {
	t          *testing.T
	l          *lifecycle
	controller *fileController
	ctx        context.Context
	root       string
}

func newContextFileFixture(t *testing.T) *contextFileFixture {
	t.Helper()
	l := contextLifecycle(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controller := newFileController("host")
	controller.contexts = l.fileContext
	return &contextFileFixture{t: t, l: l, controller: controller, ctx: localServerContext(t.Context()), root: root}
}

func (f *contextFileFixture) open(caller protocol.CallerCredential, key, cwd string) string {
	f.t.Helper()
	reply, err := f.l.contextControl(f.ctx, protocol.Control{Type: protocol.TypeContextOpen, RequestID: "open", RequestKey: key, Caller: caller, ExecutionContext: &protocol.ExecutionContextRequest{Open: &protocol.ContextSpec{Cwd: cwd}}})
	if err != nil || reply.ContextInfo == nil {
		f.t.Fatalf("open context: %v", err)
	}
	return reply.ContextInfo.ContextID
}

func (f *contextFileFixture) call(requestType string, caller protocol.CallerCredential, body any) protocol.Control {
	f.t.Helper()
	raw, err := protocol.EncodeFileDocument(body)
	if err != nil {
		f.t.Fatal(err)
	}
	response, _, err := f.controller.HandleControl(f.ctx, protocol.Control{Type: requestType, RequestID: "files", Caller: caller, Files: raw})
	if err != nil {
		f.t.Fatal(err)
	}
	return response
}

func (f *contextFileFixture) read(caller protocol.CallerCredential, contextID, path string) protocol.Control {
	return f.call(protocol.TypeFileRead, caller, protocol.FileReadRequest{Target: protocol.FileTarget{HostID: "host", ContextID: contextID}, Path: path})
}

func TestFileRelativePathsResolveInTheAuthorizedContext(t *testing.T) {
	f := newContextFileFixture(t)
	caller, _ := testCaller(t)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "sub", "a.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contextID := f.open(caller, "workspace", f.root)
	target := protocol.FileTarget{HostID: "host", ContextID: contextID}

	read := fileOK[protocol.FileReadResult](t, f.read(caller, contextID, "sub/a.txt"), protocol.TypeFileReadResult)
	if read.Data != "alpha\n" || read.Path != filepath.Join(f.root, "sub", "a.txt") || read.Target.ContextID != contextID || read.Target.Cwd != f.root {
		t.Fatalf("read: %+v", read)
	}
	// An absolute path still works inside a context; the context is a workspace, not a sandbox.
	abs := fileOK[protocol.FileReadResult](t, f.read(caller, contextID, filepath.Join(f.root, "sub", "a.txt")), protocol.TypeFileReadResult)
	if abs.Data != "alpha\n" {
		t.Fatalf("absolute read: %+v", abs)
	}
	written := fileOK[protocol.FileWriteResult](t, f.call(protocol.TypeFileWrite, caller, protocol.FileWriteRequest{Target: target, Path: "new.txt", Data: "x", CreateOnly: true}), protocol.TypeFileWriteResult)
	if written.Path != filepath.Join(f.root, "new.txt") || written.Target.Cwd != f.root {
		t.Fatalf("write: %+v", written)
	}
	patched := fileOK[protocol.FilePatchResult](t, f.call(protocol.TypeFilePatch, caller, protocol.FilePatchRequest{Target: target, Path: "new.txt", ExpectedSHA256: written.SHA256, Edits: []protocol.FileEdit{{OldText: "x", NewText: "y"}}}), protocol.TypeFilePatchResult)
	if patched.Target.ContextID != contextID {
		t.Fatalf("patch: %+v", patched)
	}
	list := fileOK[protocol.FileListResult](t, f.call(protocol.TypeFileList, caller, protocol.FileListRequest{Target: target, Path: "sub"}), protocol.TypeFileListResult)
	if list.Path != filepath.Join(f.root, "sub") || len(list.Entries) != 1 {
		t.Fatalf("list: %+v", list)
	}
	search := fileOK[protocol.FileSearchResult](t, f.call(protocol.TypeFileSearch, caller, protocol.FileSearchRequest{Target: target, Path: ".", Pattern: "alpha"}), protocol.TypeFileSearchResult)
	if search.Path != f.root || len(search.Matches) != 1 || search.Target.Cwd != f.root {
		t.Fatalf("search: %+v", search)
	}
}

func TestFileContextMustBeAuthorizedAndOpen(t *testing.T) {
	f := newContextFileFixture(t)
	alice, _ := testCaller(t)
	bob, _ := testCaller(t)
	if err := os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	contextID := f.open(alice, "workspace", f.root)

	// Another application caller never sees the context.
	fileCode(t, f.read(bob, contextID, "a.txt"), "context.not_found")
	// No credential, or a context ID that is not one, is a usage error.
	fileCode(t, f.read("", contextID, "a.txt"), "usage.invalid")
	fileCode(t, f.read(alice, "not-an-id", "a.txt"), "usage.invalid")
	fileCode(t, f.read(alice, "00000000000000000000000000000000", "a.txt"), "context.not_found")
	// A relative path needs a context.
	fileCode(t, f.read(alice, "", "a.txt"), "usage.invalid")
	// A host that cannot resolve contexts refuses rather than ignoring the field.
	f.controller.contexts = nil
	fileCode(t, f.read(alice, contextID, "a.txt"), "usage.invalid")
	f.controller.contexts = f.l.fileContext

	// A closed context stops resolving paths at once.
	if _, err := f.l.contextControl(f.ctx, protocol.Control{Type: protocol.TypeContextClose, RequestID: "close", Caller: alice, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: contextID}}); err != nil {
		t.Fatal(err)
	}
	fileCode(t, f.read(alice, contextID, "a.txt"), "context.closed")
	fileCode(t, f.call(protocol.TypeFileWrite, alice, protocol.FileWriteRequest{Target: protocol.FileTarget{HostID: "host", ContextID: contextID}, Path: "b.txt", Data: "x", CreateOnly: true}), "context.closed")
	if _, err := os.Stat(filepath.Join(f.root, "b.txt")); err == nil {
		t.Fatal("a write in a closed context created a file")
	}
}

func TestFileContextCannotNameAnotherHost(t *testing.T) {
	f := newContextFileFixture(t)
	alice, _ := testCaller(t)
	contextID := f.open(alice, "workspace", f.root)
	fileCode(t, f.call(protocol.TypeFileRead, alice, protocol.FileReadRequest{Target: protocol.FileTarget{HostID: "elsewhere", ContextID: contextID}, Path: "a.txt"}), "host.identity_mismatch")
}
