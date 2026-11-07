package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAccessOutputStrictRawBoundary(t *testing.T) {
	valid := `{"type":"access.output","requestId":"output-1","accessOutput":{"sessionId":"7K3D","format":"preview"}}`
	request, err := DecodeAccessOutput([]byte(valid))
	if err != nil || request.Output.Cols != 80 || request.Output.Rows != 24 {
		t.Fatalf("default preview: %+v %v", request, err)
	}
	for _, replacement := range []string{
		`"format":"tail","cols":0`, `"format":"tail","rows":1`, `"format":"preview","maxBytes":0`,
		`"format":"preview","cols":0`, `"format":"preview","rows":25`, `"format":"preview","cols":161`,
		`"format":"preview","cols":1,"cols":2`, `"format":"preview","Cols":1`,
		`"format":"preview","actor":"owner"`, `"format":"preview","inspection":{}`, `"format":null`,
		`"format":"tail","maxBytes":1048577`, `"format":"tail","maxBytes":-1`, `"format":"tail","maxBytes":"1"`,
		`"format":"pipe"`, `"format":"tail","maxBytes":null`,
	} {
		raw := strings.Replace(valid, `"format":"preview"`, replacement, 1)
		if _, err := DecodeAccessOutput([]byte(raw)); err == nil {
			t.Errorf("invalid output admitted: %s", raw)
		}
	}
	for _, raw := range []string{
		strings.Replace(valid, `"type":"access.output"`, `"type":"access.output","type":"access.read"`, 1),
		strings.Replace(valid, `"sessionId":"7K3D"`, `"sessionId":"7k3d"`, 1),
		strings.Replace(valid, `"accessOutput":`, `"principal":"forged","accessOutput":`, 1),
		strings.Replace(valid, `"requestId":"output-1"`, `"requestId":null`, 1), valid + `{}`, "{\xff}",
	} {
		if _, err := AccessRequestType([]byte(raw)); err == nil {
			t.Errorf("malformed request admitted: %s", raw)
		}
	}
	tail, err := DecodeAccessOutput([]byte(strings.Replace(valid, `"preview"`, `"tail"`, 1)))
	if err != nil || tail.Output.MaxBytes != DefaultLogTail {
		t.Fatal("tail default", tail, err)
	}
}
func outputTestSubject() AccessSubject {
	return AccessSubject{TargetID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", Principal: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", Class: "scoped", Permissions: []string{"metadata", "session-output"}, ExpiresAt: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
}
func TestAccessOutputMaximumBinaryTailAndVariantValidation(t *testing.T) {
	observed := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x00\xff", MaxLogTail/2)))
	request := AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: MaxLogTail}
	result := AccessOutputResult{Subject: outputTestSubject(), SessionID: "7K3D", Format: "tail", Source: "saved-tail", ObservedAt: &observed, BytesBase64: &encoded, Preview: []string{}, StyledPreview: []PreviewLine{}}
	reply := AccessOutputReply{Type: TypeAccessOutputResult, RequestID: strings.Repeat("x", MaximumAccessRequestID), Result: &result}
	raw, err := reply.Encode(request)
	if err != nil || len(raw) > MaximumAccessOutputReply {
		t.Fatal("maximum tail", len(raw), err)
	}
	if _, err := DecodeAccessOutputReply(raw, request); err != nil {
		t.Fatal(err)
	}
	result.CheckpointAt = &observed
	if _, err := reply.Encode(request); err == nil {
		t.Fatal("tail accepted checkpoint time")
	}
	result.CheckpointAt = nil
	if _, err := DecodeAccessOutputReply(raw, AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: 1}); err == nil {
		t.Fatal("reply exceeded requested bytes")
	}
	for _, extra := range []string{`"recovery":{},`, `"actor":"owner",`, `"requestId":"duplicate",`} {
		data := append([]byte("{"+extra), raw[1:]...)
		if _, err := DecodeAccessOutputReply(data, request); err == nil {
			t.Fatal("reply accepted extra/duplicate", extra)
		}
	}
}
func TestAccessOutputPreviewChecksTextStylesAndActualTimes(t *testing.T) {
	checkpoint := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	request := AccessOutput{SessionID: "7K3D", Format: "preview", Cols: 1, Rows: 1}
	result := AccessOutputResult{Subject: outputTestSubject(), SessionID: "7K3D", Format: "preview", Source: "saved-preview", CheckpointAt: &checkpoint, Preview: []string{"a"}, StyledPreview: []PreviewLine{}}
	if err := ValidateAccessOutputResult(result, request); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"界", "ab", "\x1b[2J", strings.Repeat("\u0301", MaxInspectionTextBytes)} {
		result.Preview = []string{line}
		if err := ValidateAccessOutputResult(result, request); err == nil {
			t.Fatal("invalid preview admitted")
		}
	}
	result.Preview = []string{"a"}
	result.ObservedAt = &checkpoint
	if err := ValidateAccessOutputResult(result, request); err == nil {
		t.Fatal("saved preview fabricated live observation")
	}
	result.ObservedAt = nil
	data, err := json.Marshal(result)
	if err != nil || strings.Contains(string(data), "recovery") {
		t.Fatal("invalid DTO", err)
	}
}
