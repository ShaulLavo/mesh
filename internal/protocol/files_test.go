package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFileControlTypesAreRequestsOnly(t *testing.T) {
	for _, name := range []string{TypeFileRead, TypeFileList, TypeFileSearch, TypeFileWrite, TypeFilePatch} {
		if !IsFileControl(name) {
			t.Errorf("%s is not a file control", name)
		}
	}
	for _, name := range []string{TypeFileReadResult, TypeFileListResult, TypeFileSearchResult, TypeFileWriteResult, TypeFilePatchResult, TypeHostInfo, TypeCopyRequest, ""} {
		if IsFileControl(name) {
			t.Errorf("%s is claimed as a file request", name)
		}
	}
}

func TestFileDocumentsDecodeStrictly(t *testing.T) {
	request := FileReadRequest{Target: FileTarget{HostID: "H", Account: "a"}, Path: "/x", Offset: 1 << 40, MaxBytes: 7}
	raw, err := EncodeFileDocument(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"offset":"1099511627776"`) {
		t.Fatalf("offsets travel as decimal strings: %s", raw)
	}
	var decoded FileReadRequest
	if err := DecodeFileDocument(raw, &decoded); err != nil || decoded != request {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	for _, bad := range []string{``, `null0`, `{"path":"/x","extra":1}`, `{"path":"/x"} {}`, `{"offset":5}`, `{"maxBytes":"7"}`} {
		var into FileReadRequest
		if err := DecodeFileDocument(json.RawMessage(bad), &into); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestFileControlRidesTheControlEnvelope(t *testing.T) {
	raw, err := EncodeFileDocument(FileListRequest{Target: FileTarget{HostID: "H"}, Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Control{Type: TypeFileList, RequestID: "r", Files: raw}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeControl(payload)
	if err != nil || decoded.Type != TypeFileList || string(decoded.Files) != string(raw) {
		t.Fatalf("envelope: %+v %v", decoded, err)
	}
}

func TestFileRevisionStringSeparatesEveryComponent(t *testing.T) {
	base := FileRevisionString(1, 2, 3, 4, 5)
	for name, other := range map[string]string{
		"device": FileRevisionString(9, 2, 3, 4, 5), "inode": FileRevisionString(1, 9, 3, 4, 5), "size": FileRevisionString(1, 2, 9, 4, 5),
		"modified": FileRevisionString(1, 2, 3, 9, 5), "changed": FileRevisionString(1, 2, 3, 4, 9),
	} {
		if other == base {
			t.Errorf("a different %s gives the same revision", name)
		}
	}
	if FileRevisionString(1, 2, 3, 4, 5) != base {
		t.Fatal("revisions are not deterministic")
	}
}
