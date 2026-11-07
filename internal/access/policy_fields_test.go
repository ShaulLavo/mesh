package access

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPolicyRejectsNoncanonicalFieldNames(t *testing.T) {
	for _, field := range []string{"version", "target", "revision", "entries", "subject", "incarnation", "permissions", "createdAt", "expiresAt"} {
		t.Run(field, func(t *testing.T) { checkPolicyFieldAliases(t, field) })
	}
}

func checkPolicyFieldAliases(t *testing.T, field string) {
	t.Helper()
	store, subject, expiry := testStore(t)
	testGrant(t, store, 0, subject, expiry)
	view, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	original := `"` + field + `":`
	for _, spelling := range []string{strings.ToUpper(field), strings.ToUpper(field[:1]) + field[1:]} {
		malformed := strings.Replace(string(data), original, `"`+spelling+`":`, 1)
		if _, err := parsePolicy([]byte(malformed)); err == nil {
			t.Errorf("accepted policy field alias %q", spelling)
		}
	}
	if field != "permissions" {
		return
	}
	malformed := strings.Replace(string(data), `"permissions":["metadata"]`, `"permissions":["session-output"],"Permissions":["metadata"]`, 1)
	if _, err := parsePolicy([]byte(malformed)); err == nil {
		t.Fatal("accepted ambiguous permission fields")
	}
}
