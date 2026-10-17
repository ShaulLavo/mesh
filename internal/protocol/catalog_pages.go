package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ControlFrames keeps session inventories lossless without increasing the wire
// allocation limit. Pages describe one frozen list or snapshot, never live offsets.
func ControlFrames(message Control) ([]Frame, error) {
	frame, err := boundedControlFrame(message)
	if err == nil {
		return []Frame{frame}, nil
	}
	if !errors.Is(err, ErrPayloadTooLarge) {
		return nil, err
	}
	rows, snapshot := controlSessions(message)
	if len(rows) == 0 {
		return nil, err
	}
	base := message
	base.More = true
	base.Page = len(rows)
	setControlSessions(&base, nil, snapshot)
	envelope, err := base.Encode()
	if err != nil {
		return nil, err
	}
	// The omitted sessions field adds this syntax plus rows and commas.
	overhead := len(envelope) + len(`,"sessions":[]`)
	groups, err := catalogGroups(rows, overhead)
	if err != nil {
		return nil, err
	}
	frames := make([]Frame, len(groups))
	for i, group := range groups {
		message.Page = i
		frames[i], err = catalogPage(message, group, i < len(groups)-1, snapshot)
		if err != nil {
			return nil, err
		}
	}
	return frames, nil
}

func catalogGroups(rows []SessionInfo, overhead int) ([][]SessionInfo, error) {
	var groups [][]SessionInfo
	start, size := 0, overhead
	for i, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("protocol: encode catalog session %s: %w", row.ID, err)
		}
		if len(encoded)+overhead > MaxPayload {
			return nil, fmt.Errorf("protocol: catalog session %s exceeds the %d-byte frame limit: %w", row.ID, MaxPayload, ErrPayloadTooLarge)
		}
		if size+len(encoded)+1 > MaxPayload && i > start {
			groups = append(groups, rows[start:i])
			start, size = i, overhead
		}
		size += len(encoded) + 1
	}
	return append(groups, rows[start:]), nil
}

func controlSessions(message Control) ([]SessionInfo, bool) {
	if message.Type == TypeListed {
		return message.Sessions, false
	}
	if message.Type == TypeStateSnapshot && message.StateSnapshot != nil {
		return message.StateSnapshot.Sessions, true
	}
	return nil, false
}

func setControlSessions(message *Control, rows []SessionInfo, snapshot bool) {
	if !snapshot {
		message.Sessions = rows
		return
	}
	state := *message.StateSnapshot
	state.Sessions = rows
	message.StateSnapshot = &state
}

func catalogPage(message Control, rows []SessionInfo, more, snapshot bool) (Frame, error) {
	message.More = more
	setControlSessions(&message, rows, snapshot)
	return boundedControlFrame(message)
}

func boundedControlFrame(message Control) (Frame, error) {
	payload, err := message.Encode()
	if err != nil {
		return Frame{}, err
	}
	if len(payload) > MaxPayload {
		return Frame{}, fmt.Errorf("protocol: %s response exceeds the %d-byte frame limit: %w", message.Type, MaxPayload, ErrPayloadTooLarge)
	}
	return Frame{Kind: KindControl, Payload: payload}, nil
}

// ReadControl publishes a catalog only after its final page arrives. A failed
// transfer must not replace a client's authoritative cache with a partial list.
func ReadControl(read func() (Frame, error), write func(Frame) error) (Control, error) {
	message, err := readControlPage(read)
	if err == nil && message.Page != 0 {
		return Control{}, fmt.Errorf("protocol: catalog response did not start at page zero")
	}
	if err != nil || !message.More {
		return message, err
	}
	rows, snapshot := controlSessions(message)
	if len(rows) == 0 || message.Page != 0 {
		return Control{}, fmt.Errorf("protocol: unexpected %s continuation", message.Type)
	}
	return readCatalogPages(message, rows, snapshot, read, write)
}

func readCatalogPages(message Control, rows []SessionInfo, snapshot bool, read func() (Frame, error), write func(Frame) error) (Control, error) {
	for message.More {
		if err := continueCatalog(write, message); err != nil {
			return Control{}, err
		}
		next, err := readControlPage(read)
		if err != nil {
			return Control{}, err
		}
		if next.Type == TypeError && next.RequestID == message.RequestID {
			return next, nil
		}
		part, err := validateCatalogContinuation(message, next, snapshot)
		if err != nil {
			return Control{}, err
		}
		rows = append(rows, part...)
		message.More = next.More
		message.Page = next.Page
	}
	setControlSessions(&message, rows, snapshot)
	message.Page = 0
	return message, nil
}

func validateCatalogContinuation(message, next Control, snapshot bool) ([]SessionInfo, error) {
	if next.Type != message.Type || next.RequestID != message.RequestID || next.Page != message.Page+1 {
		return nil, fmt.Errorf("protocol: catalog continuation response mismatch")
	}
	part, nextSnapshot := controlSessions(next)
	if snapshot != nextSnapshot || len(part) == 0 {
		return nil, fmt.Errorf("protocol: empty or invalid catalog continuation")
	}
	if snapshot && message.StateSnapshot.Seq != next.StateSnapshot.Seq {
		return nil, fmt.Errorf("protocol: snapshot changed during continuation")
	}
	return part, nil
}

func continueCatalog(write func(Frame) error, message Control) error {
	frame, err := boundedControlFrame(Control{Type: TypeCatalogContinue, RequestID: message.RequestID, Page: message.Page + 1, Seq: catalogSequence(message)})
	if err != nil {
		return err
	}
	if err := write(frame); err != nil {
		return fmt.Errorf("protocol: acknowledge catalog page: %w", err)
	}
	return nil
}

// AwaitCatalogContinuation stops a producer from filling the transport queue
// while a receiver decodes a page or waits for CPU time.
func AwaitCatalogContinuation(read func() (Frame, error), catalog Control, page int) error {
	if page == 0 {
		return nil
	}
	message, err := readControlPage(read)
	if err != nil {
		return err
	}
	if message.Type != TypeCatalogContinue || message.RequestID != catalog.RequestID || message.Page != page || message.Seq != catalogSequence(catalog) || message.More {
		return fmt.Errorf("protocol: expected catalog page %d continuation for request %q", page, catalog.RequestID)
	}
	return nil
}

func catalogSequence(message Control) uint64 {
	if message.Type == TypeStateSnapshot && message.StateSnapshot != nil {
		return message.StateSnapshot.Seq
	}
	return 0
}

func readControlPage(read func() (Frame, error)) (Control, error) {
	frame, err := read()
	if err != nil {
		return Control{}, fmt.Errorf("protocol: read control response: %w", err)
	}
	if frame.Kind != KindControl {
		return Control{}, fmt.Errorf("protocol: response has frame kind %d, want control", frame.Kind)
	}
	return DecodeControl(frame.Payload)
}
