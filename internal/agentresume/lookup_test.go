package agentresume

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

func TestNativeLookupRequestsOnlyOneExactID(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	done := make(chan error, 1)
	go func() { done <- answerNativeLookup(server) }()
	event, err := lookupConversation(client, client, "opaque-id")
	if err != nil || event.ConversationID != "opaque-id" || event.Directory != "/saved-worktree" {
		t.Fatalf("lookup: %+v %v", event, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func answerNativeLookup(connection net.Conn) error {
	decoder := json.NewDecoder(connection)
	var request struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		} `json:"params"`
	}
	if err := decoder.Decode(&request); err != nil || request.Method != "initialize" {
		return fmt.Errorf("bad initialize: %v", err)
	}
	if _, err := io.WriteString(connection, "{\"id\":1,\"result\":{}}\n"); err != nil {
		return err
	}
	if err := decoder.Decode(&request); err != nil || request.Method != "initialized" {
		return fmt.Errorf("bad initialization acknowledgement: %v", err)
	}
	if err := decoder.Decode(&request); err != nil || request.Method != "thread/read" || request.Params.ThreadID != "opaque-id" || request.Params.IncludeTurns {
		return fmt.Errorf("lookup must request exact ID without turns: %+v %v", request, err)
	}
	_, err := io.WriteString(connection, "{\"id\":2,\"result\":{\"thread\":{\"id\":\"opaque-id\",\"cwd\":\"/saved-worktree\",\"preview\":\"private discarded text\"}}}\n")
	return err
}

func TestNativeLookupRejectsAliasesMissingAndUnboundedReplies(t *testing.T) {
	if _, err := decodeLookupIdentity([]byte(`{"thread":{"id":"other-id","cwd":"/project"}}`), "wanted-id"); err == nil {
		t.Fatal("lookup accepted an alias instead of exact ID")
	}
	for _, input := range []string{`{"id":2,"error":{"message":"private provider error"}}` + "\n", strings.Repeat("{}\n", 33), strings.Repeat("x", maxLookupFrameBytes+1)} {
		scanner := bufio.NewScanner(strings.NewReader(input))
		scanner.Buffer(make([]byte, 4096), maxLookupFrameBytes)
		_, err := readLookupReply(scanner, 2)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("lookup failed to bound or sanitize response: %v", err)
		}
	}
}
