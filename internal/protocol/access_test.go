package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAccessReadStrictRawBoundary(t *testing.T) {
	valid := `{"type":"access.read","requestId":"read-1","accessRead":{"section":"sessions","limit":1}}`
	if _, err := DecodeAccessRead([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		strings.Replace(valid, `"type":"access.read"`, `"type":"access.read","type":"session.attach"`, 1),
		strings.Replace(valid, `"limit":1`, `"limit":1,"limit":1`, 1),
		strings.Replace(valid, `"limit":1`, `"actor":"forged"`, 1),
		strings.Replace(valid, `"limit":1`, `"limit":0`, 1),
		strings.Replace(valid, `"limit":1`, `"limit":null`, 1),
		strings.Replace(valid, `"limit":1`, `"limit":"1"`, 1),
		strings.Replace(valid, `"limit":1`, `"limit":501`, 1),
		strings.Replace(valid, `"section":"sessions","limit":1`, `"section":"host","limit":0`, 1),
		strings.Replace(valid, `"section":"sessions","limit":1`, `"section":"host","sessionId":""`, 1),
		strings.Replace(valid, `"section":"sessions","limit":1`, `"section":"host","cursor":""`, 1),
		strings.Replace(valid, `"limit":1`, `"sessionId":"7K3D","cursor":"x"`, 1),
		strings.Replace(valid, `"limit":1`, `"preview":[]`, 1),
		strings.Replace(valid, `"accessRead":{`, `"accessRead":null,"ignored":{`, 1),
		strings.Replace(valid, `"requestId":"read-1"`, `"requestId":null`, 1),
		strings.Replace(valid, `"requestId":"read-1"`, `"requestId":"`+strings.Repeat("x", 129)+`"`, 1),
		strings.Replace(valid, `"limit":1`, `"cursor":"`+strings.Repeat("x", MaximumAccessCursor+1)+`"`, 1),
		strings.Replace(valid, `"type":"access.read"`, `"Type":"access.read"`, 1),
		strings.Replace(valid, `"requestId":"read-1"`, `"requestId":"\u0000"`, 1),
		valid + `{}`, strings.Repeat(" ", MaximumAccessRequest) + valid,
		"{\xff}", `[]`, `null`,
	}
	for index, data := range cases {
		if _, err := DecodeAccessRead([]byte(data)); err == nil {
			t.Errorf("case %d accepted: %q", index, data)
		}
	}
}
func TestAccessCursorStrictBindingEncoding(t *testing.T) {
	cursor := AccessCursor{Target: "target", Subject: "subject", Incarnation: "generation", Section: "sessions", Last: "7K3D"}
	token, err := cursor.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAccessCursor(token)
	if err != nil || got != cursor {
		t.Fatalf("cursor roundtrip: %+v %v", got, err)
	}
	for _, token := range []string{"", token + "=", "\xff", strings.Repeat("x", MaximumAccessCursor+1)} {
		if _, err := DecodeAccessCursor(token); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
}
func TestAccessReplyRejectsFieldsOutsideDTO(t *testing.T) {
	data := `{"type":"access.denied","requestId":"read","errorCode":"access.denied","message":"Metadata permission is required."}`
	if _, err := DecodeAccessReply([]byte(data)); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		t.Fatal(err)
	}
	value["recovery"] = map[string]any{"recipe": "secret"}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAccessReply(encoded); err == nil {
		t.Fatal("legacy field accepted in scoped reply")
	}
	duplicate := strings.Replace(data, `"requestId":"read"`, `"requestId":"read","requestId":"other"`, 1)
	if _, err := DecodeAccessReply([]byte(duplicate)); err == nil {
		t.Fatal("duplicate scoped reply field accepted")
	}
}
