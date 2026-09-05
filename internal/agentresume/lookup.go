package agentresume

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const maxLookupFrameBytes = 1 << 20

// LookupConversation validates one explicit Codex ID using the native read-only
// API. It neither lists conversations nor reads their turns or modifies history.
func LookupConversation(parent context.Context, recipe Recipe, inherited []string) (Event, error) {
	if err := CheckAvailable(recipe); err != nil {
		return Event{}, err
	}
	if recipe.Provider != Codex || recipe.ProviderVersion != "codex-cli 0.153.4" {
		return Event{}, fmt.Errorf("agent recovery: native ID lookup is not verified for this provider version")
	}
	env, err := ResumeEnv(recipe, inherited)
	if err != nil {
		return Event{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, recipe.Executable, "app-server", "--stdio") //nolint:gosec // the recorded provider installation implements the bounded read-only protocol
	command.Env, command.Dir, command.Stderr = env, recipe.Directory, io.Discard
	input, err := command.StdinPipe()
	if err != nil {
		return Event{}, fmt.Errorf("agent recovery: open native lookup input: %w", err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return Event{}, fmt.Errorf("agent recovery: open native lookup output: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		return Event{}, fmt.Errorf("agent recovery: start native lookup: %w", err)
	}
	defer func() {
		_ = input.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	return lookupConversation(input, output, recipe.ConversationID)
}

func lookupConversation(input io.Writer, output io.Reader, expectedID string) (Event, error) {
	encoder := json.NewEncoder(input)
	initialize := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "mesh-agent-recovery", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}}
	if err := encoder.Encode(initialize); err != nil {
		return Event{}, fmt.Errorf("agent recovery: initialize native lookup: %w", err)
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), maxLookupFrameBytes)
	if _, err := readLookupReply(scanner, 1); err != nil {
		return Event{}, err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return Event{}, fmt.Errorf("agent recovery: acknowledge native initialization: %w", err)
	}
	request := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{2, "thread/read", map[string]any{"threadId": expectedID, "includeTurns": false}}
	if err := encoder.Encode(request); err != nil {
		return Event{}, fmt.Errorf("agent recovery: read exact native conversation: %w", err)
	}
	result, err := readLookupReply(scanner, 2)
	if err != nil {
		return Event{}, err
	}
	return decodeLookupIdentity(result, expectedID)
}

func readLookupReply(scanner *bufio.Scanner, expected int) (json.RawMessage, error) {
	for range 32 {
		if !scanner.Scan() {
			return nil, fmt.Errorf("agent recovery: native lookup ended without a bounded reply")
		}
		var reply struct {
			ID     *int            `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
			return nil, fmt.Errorf("agent recovery: invalid native lookup reply")
		}
		if reply.ID == nil || *reply.ID != expected {
			continue
		}
		if len(reply.Error) > 0 && string(reply.Error) != "null" {
			return nil, fmt.Errorf("agent recovery: provider rejected the exact conversation ID or lookup request")
		}
		return reply.Result, nil
	}
	return nil, fmt.Errorf("agent recovery: native lookup exceeded its reply limit")
}

func decodeLookupIdentity(raw json.RawMessage, expected string) (Event, error) {
	var result struct {
		Thread struct {
			ID        string `json:"id"`
			Directory string `json:"cwd"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Thread.ID != expected {
		return Event{}, fmt.Errorf("agent recovery: provider did not return the exact requested conversation")
	}
	event := Event{Kind: Start, ConversationID: result.Thread.ID, Directory: result.Thread.Directory}
	if err := ValidateEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}
