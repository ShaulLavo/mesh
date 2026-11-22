package daemon

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

// A file control that carries an application credential but names no context
// has nothing to authorize, so the credential must be refused rather than
// ignored: a frontend must never think an account-wide call was isolated.
func TestFileControlsRefuseACredentialWithoutAContext(t *testing.T) {
	f := newContextFileFixture(t)
	caller, _ := testCaller(t)
	if err := os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "a.txt")
	target := protocol.FileTarget{HostID: "host"}

	// Through the client server, as a frontend sends it.
	server, err := newClientServer(f.l, &contextWorkerCounter{}, &contextControlCounter{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.files = f.controller
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(f.ctx, client) }()
	raw, err := protocol.EncodeFileDocument(protocol.FileReadRequest{Target: target, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeFileRead, RequestID: "labelled", Caller: caller, Files: raw}))
	fileCode(t, decodeServerControl(t, client.nextWrite(t)), "usage.invalid")
	// An ordinary credential-free absolute request still works.
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeFileRead, RequestID: "plain", Files: raw}))
	if page := fileOK[protocol.FileReadResult](t, decodeServerControl(t, client.nextWrite(t)), protocol.TypeFileReadResult); page.Data != "alpha\n" {
		t.Fatalf("credential-free read: %+v", page)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "caller server"); err != nil {
		t.Fatal(err)
	}

	// Fields that belong to other controls are refused on every file control.
	for name, mutate := range map[string]func(*protocol.Control){
		"administration": func(c *protocol.Control) { c.Administration = true },
		"execution context": func(c *protocol.Control) {
			c.ExecutionContext = &protocol.ExecutionContextRequest{ContextID: "0123456789abcdef0123456789abcdef"}
		},
	} {
		request := protocol.Control{Type: protocol.TypeFileRead, RequestID: name, Files: raw}
		mutate(&request)
		response, _, err := f.controller.HandleControl(f.ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		fileCode(t, response, "usage.invalid")
	}
}

// The account a daemon puts in host.info is the account its file controller
// checks, so a frontend can always name it.
func TestHostInfoAndFileControllerShareTheAccountName(t *testing.T) {
	l := contextLifecycle(t)
	account := l.declaredHostInfo().Account
	if account == "" || account != newFileController("host").account {
		t.Fatalf("host.info account %q, file controller %q", account, newFileController("host").account)
	}
}

// Once a context is authorized, an error still tells the caller which
// workspace the request ran in; before that point it must not.
func TestFileErrorsAfterAuthorizationCarryTheWorkspace(t *testing.T) {
	f := newContextFileFixture(t)
	alice, _ := testCaller(t)
	bob, _ := testCaller(t)
	if err := os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	contextID := f.open(alice, f.root)
	conflict := f.call(protocol.TypeFileWrite, alice, protocol.FileWriteRequest{Target: protocol.FileTarget{HostID: "host", ContextID: contextID}, Path: "a.txt", Data: "x", ExpectedSHA256: digestHex([]byte("different"))})
	fileCode(t, conflict, "file.conflict")
	var detail struct {
		Target protocol.FileTarget `json:"target"`
	}
	if err := json.Unmarshal(conflict.Files, &detail); err != nil || detail.Target.ContextID != contextID || detail.Target.Cwd != f.root || detail.Target.HostID != "host" {
		t.Fatalf("an authorized failure must name its workspace: %s (%v)", conflict.Files, err)
	}
	denied := f.read(bob, contextID, "a.txt")
	fileCode(t, denied, "context.not_found")
	if len(denied.Files) != 0 {
		t.Fatalf("an unauthorized context revealed a workspace: %s", denied.Files)
	}
}
