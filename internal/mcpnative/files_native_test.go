package mcpnative

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
)

const testContextHandle = "mc1." + testHostID + ".0123456789abcdef0123456789abcdef"

// fileStub records the last M6 request a handler produced and answers with a
// canned outcome. It is embedded in stubControls.
type fileStub struct {
	last    any
	outcome *CallOutcome
}

func (s *fileStub) reply(req any, operation string, data any) (CallOutcome, error) {
	s.last = req
	if s.outcome != nil {
		return *s.outcome, nil
	}
	target := &TargetMeta{HostID: testHostID, HostName: "pc", Account: "agent"}
	return CallOutcome{Envelope: okEnvelope(operation, data), Target: target}, nil
}

func (s *fileStub) FileRead(_ context.Context, req FileReadRequest) (CallOutcome, error) {
	return s.reply(req, "files.read", FileReadData{Path: req.Path, Encoding: "utf-8", Data: "hello\x1b[31m\n", Start: "0", End: "9", Size: "9", EOF: true, Revision: "r1", SHA256: strings.Repeat("a", 64)})
}
func (s *fileStub) FileList(_ context.Context, req FileListRequest) (CallOutcome, error) {
	return s.reply(req, "files.list", FileListData{Path: req.Path, Entries: []FileEntryData{{Name: "a\x1b", Kind: "file"}}})
}
func (s *fileStub) FileSearch(_ context.Context, req FileSearchRequest) (CallOutcome, error) {
	return s.reply(req, "files.search", FileSearchData{Path: req.Path, Matches: []FileMatchData{{Path: req.Path, Line: 1, Column: 2, Text: "x"}}, Truncated: "matches_limit"})
}
func (s *fileStub) FileWrite(_ context.Context, req FileWriteRequest) (CallOutcome, error) {
	return s.reply(req, "files.write", FileWriteData{Path: req.Path, Size: "3", SHA256: strings.Repeat("b", 64), Revision: "r2", Created: req.CreateOnly})
}
func (s *fileStub) FilePatch(_ context.Context, req FilePatchRequest) (CallOutcome, error) {
	return s.reply(req, "files.patch", FileWriteData{Path: req.Path, Size: "3", SHA256: strings.Repeat("c", 64), Revision: "r3", Edits: len(req.Edits)})
}
func (s *fileStub) Copy(_ context.Context, req CopyRequest) (CallOutcome, error) {
	return s.reply(req, "files.copy", CopyData{Direction: req.Direction, LocalPath: req.LocalPath, RemotePath: req.RemotePath, Bytes: "12"})
}
func (s *fileStub) FleetStatus(_ context.Context, req FleetStatusRequest) (CallOutcome, error) {
	return s.reply(req, "profile.status", agentapi.FleetStatus{Fleet: "home", Members: []agentapi.Delivery{}})
}
func (s *fileStub) ProfileGet(_ context.Context, req ProfileGetRequest) (CallOutcome, error) {
	return s.reply(req, "profile.show", agentapi.ProfileSection{State: "absent"})
}
func (s *fileStub) ProfileSet(_ context.Context, req ProfileSetRequest) (CallOutcome, error) {
	return s.reply(req, "profile.set", agentapi.ProfileWrite{Revision: req.ExpectedRevision + 1, Committed: true, Members: []agentapi.Delivery{}})
}

func callFileTool(t *testing.T, stub *stubControls, tool, args string) (CallOutcome, error) {
	t.Helper()
	spec, ok := DefaultRegistry().Lookup(tool)
	if !ok {
		t.Fatalf("%s is not registered", tool)
	}
	return spec.Handler(t.Context(), stub, json.RawMessage(args), nil)
}

func requireInvalid(t *testing.T, tool, args, want string) {
	t.Helper()
	_, err := callFileTool(t, &stubControls{}, tool, args)
	var invalid *paramError
	if !errors.As(err, &invalid) || !strings.Contains(invalid.message, want) {
		t.Fatalf("%s %s: error %v, want invalid params containing %q", tool, args, err, want)
	}
}

func TestFileToolsAreRegisteredWithLinkedOperationsAndHints(t *testing.T) {
	want := map[string]bool{ // tool -> read only
		"mesh_file_read": true, "mesh_file_list": true, "mesh_file_search": true, "mesh_fleet_status": true, "mesh_profile_get": true,
		"mesh_file_write": false, "mesh_file_patch": false, "mesh_copy": false, "mesh_profile_set": false,
	}
	for _, def := range DefaultRegistry().Definitions() {
		readOnly, listed := want[def.Name]
		if !listed {
			continue
		}
		delete(want, def.Name)
		if def.Operation == "" || def.Annotations.ReadOnlyHint == nil || *def.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("%s: operation %q read-only %v", def.Name, def.Operation, def.Annotations.ReadOnlyHint)
		}
		if !readOnly && (def.Annotations.DestructiveHint == nil || !*def.Annotations.DestructiveHint) {
			t.Errorf("%s must be annotated destructive", def.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("tools missing from the registry: %v", want)
	}
}

func TestFileReadArgumentsMapToTheRequest(t *testing.T) {
	stub := &stubControls{}
	out, err := callFileTool(t, stub, "mesh_file_read", `{"host":" pc ","path":"/work/a.txt","offset":"4096","max_bytes":100,"digest":true,"timeout_ms":2000}`)
	if err != nil || out.Envelope.Status != agentapi.StatusOK {
		t.Fatalf("read: %+v %v", out, err)
	}
	req := stub.last.(FileReadRequest)
	if req.Target.Host != "pc" || req.Target.Context != nil || req.Path != "/work/a.txt" || req.Offset != 4096 || req.MaxBytes != 100 || !req.Digest || req.Timeout.Milliseconds() != 2000 {
		t.Fatalf("request: %+v", req)
	}
	// A context handle names the host by itself and permits a relative path.
	if _, err := callFileTool(t, stub, "mesh_file_read", `{"context":"`+testContextHandle+`","path":"src/main.go","cursor":"abc"}`); err != nil {
		t.Fatal(err)
	}
	req = stub.last.(FileReadRequest)
	if req.Target.Host != "" || req.Target.Context == nil || req.Target.Context.HostID != testHostID || req.Target.Context.ContextID != "0123456789abcdef0123456789abcdef" || req.Cursor != "abc" || req.Path != "src/main.go" {
		t.Fatalf("context request: %+v", req)
	}
	if req.Target.Context.String() != testContextHandle {
		t.Fatalf("handle does not round trip: %s", req.Target.Context)
	}
}

func TestFileToolsRejectMalformedArguments(t *testing.T) {
	for _, test := range []struct{ tool, args, want string }{
		{"mesh_file_read", `{"path":"/a"}`, "host is required"},
		{"mesh_file_read", `{"host":"pc"}`, "path must be"},
		{"mesh_file_read", `{"host":"pc","path":"relative.txt"}`, "absolute"},
		{"mesh_file_read", `{"host":"pc","path":"/a","offset":"-1"}`, "decimal"},
		{"mesh_file_read", `{"host":"pc","path":"/a","offset":"01"}`, "decimal"},
		{"mesh_file_read", `{"host":"pc","path":"/a","offset":"5","cursor":"x"}`, "not both"},
		{"mesh_file_read", `{"host":"pc","path":"/a","max_bytes":1048577}`, "max_bytes"},
		{"mesh_file_read", `{"host":"pc","path":"/a","max_bytes":0}`, "max_bytes"},
		{"mesh_file_read", `{"host":"pc","path":"/a","cursor":"` + strings.Repeat("x", maxFileCursorLength+1) + `"}`, "cursor"},
		{"mesh_file_read", `{"context":"mc1.bad.0123456789abcdef0123456789abcdef","path":"a"}`, "context"},
		{"mesh_file_read", `{"context":"mj1.` + testHostID + `.0123456789abcdef0123456789abcdef","path":"a"}`, "context"},
		{"mesh_file_read", `{"host":"pc","path":"/a","extra":1}`, "invalid tool arguments"},
		{"mesh_file_list", `{"host":"pc","path":"/","limit":1001}`, "limit"},
		{"mesh_file_search", `{"host":"pc","path":"/"}`, "pattern"},
		{"mesh_file_search", `{"host":"pc","path":"/","pattern":"x","max_matches":1001}`, "max_matches"},
		{"mesh_file_write", `{"host":"pc","path":"/a","create_only":true}`, "exactly one of content"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content":"x","content_base64":"eA==","create_only":true}`, "exactly one of content"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content":"x"}`, "expected_sha256 and create_only"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content":"x","create_only":true,"expected_sha256":"` + strings.Repeat("a", 64) + `"}`, "expected_sha256 and create_only"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content":"x","expected_sha256":"ABC"}`, "expected_sha256"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content_base64":"!!","create_only":true}`, "base64"},
		{"mesh_file_write", `{"host":"pc","path":"/a","content":"` + strings.Repeat("x", protocol.MaxFileWrite+1) + `","create_only":true}`, "exceeds"},
		{"mesh_file_patch", `{"host":"pc","path":"/a","expected_sha256":"` + strings.Repeat("a", 64) + `","edits":[]}`, "edits"},
		{"mesh_file_patch", `{"host":"pc","path":"/a","expected_sha256":"` + strings.Repeat("a", 64) + `","edits":[{"old_text":"","new_text":"x"}]}`, "old_text"},
		{"mesh_file_patch", `{"host":"pc","path":"/a","edits":[{"old_text":"a","new_text":"x"}]}`, "expected_sha256"},
		{"mesh_copy", `{"host":"pc","direction":"sideways","local_path":"/a","remote_path":"/b"}`, "direction"},
		{"mesh_copy", `{"host":"pc","direction":"upload","local_path":"a","remote_path":"/b"}`, "local_path"},
		{"mesh_copy", `{"host":"pc","direction":"upload","local_path":"/a","remote_path":"b"}`, "remote_path"},
		{"mesh_fleet_status", `{"host":"pc"}`, "invalid tool arguments"},
		{"mesh_profile_get", `{"scope":"galaxy"}`, "scope"},
		{"mesh_profile_set", `{"profile":{"schemaVersion":1}}`, "expected_revision"},
		{"mesh_profile_set", `{"expected_revision":0}`, "profile must be an object"},
		{"mesh_profile_set", `{"expected_revision":0,"profile":"text"}`, "profile must be an object"},
		{"mesh_profile_set", `{"expected_revision":-1,"profile":{"schemaVersion":1}}`, "invalid tool arguments"},
	} {
		requireInvalid(t, test.tool, test.args, test.want)
	}
}

func TestFileWriteBoundsAndEncodings(t *testing.T) {
	stub := &stubControls{}
	exact := strings.Repeat("x", protocol.MaxFileWrite)
	if _, err := callFileTool(t, stub, "mesh_file_write", `{"host":"pc","path":"/a","content":"`+exact+`","create_only":true}`); err != nil {
		t.Fatalf("a write of exactly the limit was refused: %v", err)
	}
	if got := stub.last.(FileWriteRequest); len(got.Data) != protocol.MaxFileWrite || !got.CreateOnly {
		t.Fatalf("request: %d bytes", len(got.Data))
	}
	binary := []byte{0, 1, 0xff, 0xfe}
	args := `{"host":"pc","path":"/a","content_base64":"` + base64.StdEncoding.EncodeToString(binary) + `","expected_sha256":"` + strings.Repeat("d", 64) + `"}`
	if _, err := callFileTool(t, stub, "mesh_file_write", args); err != nil {
		t.Fatal(err)
	}
	if got := stub.last.(FileWriteRequest); string(got.Data) != string(binary) || got.ExpectedSHA256 != strings.Repeat("d", 64) || got.CreateOnly {
		t.Fatalf("binary request: %+v", got)
	}
	tooLarge := base64.StdEncoding.EncodeToString(make([]byte, protocol.MaxFileWrite+1))
	requireInvalid(t, "mesh_file_write", `{"host":"pc","path":"/a","content_base64":"`+tooLarge+`","create_only":true}`, "exceeds")
}

func TestFilePatchSearchListAndCopyArgumentsMap(t *testing.T) {
	stub := &stubControls{}
	digest := strings.Repeat("e", 64)
	if _, err := callFileTool(t, stub, "mesh_file_patch", `{"host":"pc","path":"/a","expected_sha256":"`+digest+`","edits":[{"old_text":"a","new_text":"b"},{"old_text":"c","new_text":""}]}`); err != nil {
		t.Fatal(err)
	}
	patch := stub.last.(FilePatchRequest)
	if len(patch.Edits) != 2 || patch.Edits[0] != (FileEditRequest{OldText: "a", NewText: "b"}) || patch.Edits[1].NewText != "" || patch.ExpectedSHA256 != digest {
		t.Fatalf("patch: %+v", patch)
	}
	if _, err := callFileTool(t, stub, "mesh_file_search", `{"host":"pc","path":"/","pattern":"fo+","glob":"*.go","max_matches":5}`); err != nil {
		t.Fatal(err)
	}
	if search := stub.last.(FileSearchRequest); search.Pattern != "fo+" || search.Glob != "*.go" || search.MaxMatches != 5 {
		t.Fatalf("search: %+v", search)
	}
	if _, err := callFileTool(t, stub, "mesh_file_list", `{"host":"pc","path":"/","limit":10,"cursor":"c"}`); err != nil {
		t.Fatal(err)
	}
	if list := stub.last.(FileListRequest); list.Limit != 10 || list.Cursor != "c" {
		t.Fatalf("list: %+v", list)
	}
	if _, err := callFileTool(t, stub, "mesh_copy", `{"host":"pc","direction":"download","local_path":"/tmp/x","remote_path":"/srv/y","recursive":true}`); err != nil {
		t.Fatal(err)
	}
	if copyReq := stub.last.(CopyRequest); copyReq.Direction != CopyDownload || !copyReq.Recursive || copyReq.Timeout.Milliseconds() != defaultCopyMS {
		t.Fatalf("copy: %+v", copyReq)
	}
	if _, err := callFileTool(t, stub, "mesh_profile_set", `{"scope":"fleet","expected_revision":3,"profile":{"schemaVersion":1,"description":"d"}}`); err != nil {
		t.Fatal(err)
	}
	if set := stub.last.(ProfileSetRequest); set.Scope != ProfileScopeFleet || set.ExpectedRevision != 3 || !strings.Contains(string(set.Document), `"description"`) {
		t.Fatalf("profile set: %+v", set)
	}
	if _, err := callFileTool(t, stub, "mesh_profile_get", `{}`); err != nil {
		t.Fatal(err)
	}
	if get := stub.last.(ProfileGetRequest); get.Scope != ProfileScopeHost {
		t.Fatalf("profile get: %+v", get)
	}
}

func TestFileResultsRenderWithTargetAndEscapedText(t *testing.T) {
	out, err := callFileTool(t, &stubControls{}, "mesh_file_read", `{"host":"pc","path":"/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := renderOutcomeText(out.Envelope, false)
	if strings.Contains(text, "\x1b") || !strings.Contains(text, `\u001b[31m`) || !strings.Contains(text, "revision=r1") {
		t.Fatalf("read text: %q", text)
	}
	list, _ := callFileTool(t, &stubControls{}, "mesh_file_list", `{"host":"pc","path":"/"}`)
	if text := renderOutcomeText(list.Envelope, false); strings.Contains(text, "\x1b") || !strings.Contains(text, "file a") {
		t.Fatalf("list text: %q", text)
	}
	search, _ := callFileTool(t, &stubControls{}, "mesh_file_search", `{"host":"pc","path":"/","pattern":"x"}`)
	if text := renderOutcomeText(search.Envelope, false); !strings.Contains(text, "stopped at matches_limit") {
		t.Fatalf("search text: %q", text)
	}
	failed := errorEnvelope("files.write", "/a", agentapi.FileConflict, "digest differs")
	if text := renderOutcomeText(failed, true); !strings.Contains(text, "[file.conflict]") || !strings.Contains(text, "Next:") {
		t.Fatalf("error text: %q", text)
	}
}

func TestFileToolCallOverTheWireCarriesTheTargetMeta(t *testing.T) {
	out, reader, _, _ := startTestServer(t, ServerOptions{Version: "0.1.0-test", Controls: &stubControls{}})
	call := mcpReq("1", "tools/call", `,"name":"mesh_file_write","arguments":{"host":"pc","path":"/work/a.txt","content":"hi","create_only":true}`)
	if err := mcpwire.WriteStdio(out, call); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent agentapi.Envelope `json:"structuredContent"`
			Meta              map[string]any    `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	target, _ := reply.Result.Meta[MetaTargetKey].(map[string]any)
	if reply.Result.IsError || reply.Result.StructuredContent.Operation != "files.write" || target["hostId"] != testHostID || target["account"] != "agent" {
		t.Fatalf("reply: %s", raw)
	}
}
