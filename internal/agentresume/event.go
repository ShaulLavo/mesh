package agentresume

import (
	"encoding/json"
	"fmt"
	"io"
)

// DecodeEvent discards provider context, transcript paths and prompt contents.
func DecodeEvent(provider Provider, input io.Reader) (Event, error) {
	if err := ValidateProvider(provider); err != nil {
		return Event{}, err
	}
	contents, err := io.ReadAll(io.LimitReader(input, MaxEventBytes+1))
	if err != nil || len(contents) > MaxEventBytes {
		return Event{}, fmt.Errorf("agent recovery: cannot read bounded hook event")
	}
	var wire struct {
		Name      string `json:"hook_event_name"`
		ID        string `json:"session_id"`
		Directory string `json:"cwd"`
		AgentID   string `json:"agent_id"`
		AgentType string `json:"agent_type"`
		ParentID  string `json:"parent_session_id"`
	}
	if err := json.Unmarshal(contents, &wire); err != nil {
		return Event{}, fmt.Errorf("agent recovery: malformed hook event")
	}
	event := Event{ConversationID: wire.ID, Directory: wire.Directory,
		Subagent: wire.AgentID != "" || wire.AgentType != "" || wire.ParentID != ""}
	switch wire.Name {
	case "SessionStart":
		event.Kind = Start
	case "SessionEnd":
		event.Kind = End
	case "SubagentStart", "SubagentStop":
		event.Kind, event.Subagent = Start, true
	default:
		return Event{}, fmt.Errorf("agent recovery: unsupported hook event")
	}
	if err := ValidateEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}
