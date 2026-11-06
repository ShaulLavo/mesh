package protocol

import "testing"

func TestFleetSyncRejectsPayloadBeforeUnknownFieldsCanDisappear(t *testing.T) {
	for _, field := range []string{`"document":{}`, `"target":"host"`, `"actor":"author"`, `"context":null`, `"update":{}`} {
		input := []byte(`{"type":"fleet.profile.sync","requestId":"sync",` + field + `}`)
		if _, err := DecodeControl(input); err == nil {
			t.Fatalf("sync accepted %s", input)
		}
	}
	if _, err := DecodeControl([]byte(`{"type":"fleet.profile.sync","requestId":"sync"}`)); err != nil {
		t.Fatal(err)
	}
}
