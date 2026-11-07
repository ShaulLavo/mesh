package agentapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRequestedAndClassification(t *testing.T) {
	for _, test := range []struct {
		args      []string
		requested bool
		operation string
	}{
		{[]string{"serve", "ls", "--json"}, true, "services.list"},
		{[]string{"--privacy", "serve", "--json", "list"}, true, "services.list"},
		{[]string{"serve", "list", "--json=true", "--json=false"}, false, "services.list"},
		{[]string{"serve", "ls", "--json=false", "--json"}, true, "services.list"},
		{[]string{"serve", "ls", "--json=1"}, true, "services.list"},
		{[]string{"serve", "ls", "--json", "--leave-key", "--json=false"}, true, "services.list"},
		{[]string{"serve", "ls", "--leave-key", "--json"}, false, "services.list"},
		{[]string{"--leave-key", "--", "serve", "ls", "--json"}, true, "services.list"},
		{[]string{"--host", "--json", "serve", "ls"}, false, "services.list"},
		{[]string{"--detach-key", "--json", "serve", "ls"}, false, "services.list"},
		{[]string{"serve", "ls", "--timeout", "--json"}, false, "services.list"},
		{[]string{"serve", "ls", "--json=invalid"}, true, "services.list"},
		{[]string{"serve", "ls", "--json=invalid", "--json=false"}, true, "services.list"},
		{[]string{"serve", "ls", "--", "--json"}, false, "services.list"},
		{[]string{"--host", "pc", "--", "tool", "--json"}, false, ""},
		{[]string{"--host=pc", "--", "serve", "ls", "--json"}, false, ""},
		{[]string{"profile", "show", "--json"}, true, "profile.show"},
		{[]string{"profile", "nope", "--json"}, true, "profile.show"},
		{[]string{"profile", "--json"}, true, "profile.show"},
		{[]string{"profile", "set", "--file", "--json", "--expected-revision", "0"}, false, "profile.set"},
		{[]string{"profile", "set", "--json", "--file", "--json=false", "--expected-revision", "0"}, true, "profile.set"},
		{[]string{"profile", "edit", "--json", "--", "--json=false"}, true, "profile.edit"},
		{[]string{"hosts", "--json"}, true, "hosts.list"},
		{[]string{"inspect", "HOST_ID", "--json"}, true, "hosts.inspect"},
		{[]string{"inspect", "--timeout", "--json"}, false, "hosts.inspect"},
		{[]string{"skill", "nope", "--json"}, true, "skill.status"},
		{[]string{"skill", "show", "--file", "--json"}, false, "skill.show"},
		{[]string{"skill", "status", "--dir", "--json"}, false, "skill.status"},
		{[]string{"skill", "status", "--dir", "--json=false", "--json"}, true, "skill.status"},
		{[]string{"--dir", "device", "skill", "show"}, false, "skill.show"},
		{[]string{"device", "scope", "keygen", "--key-dir", "--json"}, false, "scope.keygen"},
		{[]string{"device", "scope", "grant", "--identity", "--json", "--json"}, true, "scope.grant"},
		{[]string{"device", "scope", "revoke", "--expected-revision", "--json"}, false, "scope.revoke"},
		{[]string{"device", "scope", "list", "--json"}, true, "scope.list"},
		{[]string{"device", "scope", "read", "--section", "--json"}, false, "scope.read"},
		{[]string{"device", "scope", "read", "--json", "--", "--json=false"}, true, "scope.read"},
		{[]string{"version", "--json"}, true, ""},
	} {
		if got := Requested(test.args); got != test.requested {
			t.Errorf("%v requested %v", test.args, got)
		}
		if got := Classify(test.args); got != test.operation {
			t.Errorf("%v operation %q", test.args, got)
		}
	}
}

func TestEnvelopeStatusAndCodes(t *testing.T) {
	for code, class := range Codes {
		checkErrorClass(t, code, class)
	}
	for _, test := range []struct {
		status Status
		errors []Error
		exit   int
	}{
		{StatusOK, nil, 0}, {StatusPartial, []Error{Usage("services.list", "invalid").Err}, 3},
		{StatusError, []Error{Usage("services.list", "invalid").Err}, 2}, {StatusError, nil, 1},
	} {
		if got := (Envelope{Status: test.status, Errors: test.errors}).ExitCode(); got != test.exit {
			t.Fatalf("wrong exit %d", got)
		}
	}
}

func checkErrorClass(t *testing.T, code Code, class ErrorClass) {
	t.Helper()
	failure := Fail(code, "services.list", "HOST_ID", "failed")
	if class.Reserved {
		if failure.Err.Code != InternalError {
			t.Fatal("emitted reserved code")
		}
		return
	}
	if failure.Exit != class.Exit || failure.Err.Retryable != class.Retryable {
		t.Fatalf("wrong class for %s", code)
	}
}

func TestEnvelopeEncodingAndBoundedErrors(t *testing.T) {
	var output bytes.Buffer
	message := "\x1b\n\u202e" + strings.Repeat("é", 1000)
	err := Write(&output, Envelope{SchemaVersion: 1, Status: StatusError, Data: nil, Errors: []Error{Fail(StateUnavailable, "services.list", "", message).Err}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	decoder := json.NewDecoder(&output)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Errors[0].Message) > 512 || !utf8.ValidString(envelope.Errors[0].Message) || strings.ContainsAny(envelope.Errors[0].Message, "\x1b\n\u202e") {
		t.Fatal("unsafe error text")
	}
	if err := decoder.Decode(&envelope); err != io.EOF {
		t.Fatal("more than one object")
	}
	output.Reset()
	if err := Write(&output, Envelope{Status: StatusOK, Data: "<&>"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"errors":[]`) || !strings.Contains(output.String(), "<&>") {
		t.Fatal(output.String())
	}
}

func TestRequestIDs(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id, err := NewRequestID()
		if err != nil || len(id) != 26 || id[0] > '7' || seen[id] {
			t.Fatalf("invalid request ID %q: %v", id, err)
		}
		seen[id] = true
		if strings.Trim(id, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") != "" {
			t.Fatal("not Crockford base32")
		}
	}
}
