package protocol

import "fmt"

// MaxNestedSessions bounds live registrations, including duplicate identities.
const MaxNestedSessions = MaxContainingSessions

// ValidateNestedSessions checks the unique set advertised by a worker.
func ValidateNestedSessions(identities []SessionIdentity) error {
	if len(identities) > MaxNestedSessions {
		return fmt.Errorf("protocol: nesting has %d sessions; maximum is %d", len(identities), MaxNestedSessions)
	}
	seen := make(map[SessionIdentity]struct{}, len(identities))
	for index, identity := range identities {
		if err := ValidateSessionIdentity(identity); err != nil {
			return fmt.Errorf("protocol: nested session %d: %w", index, err)
		}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("protocol: nested session %d duplicates %s/%s", index, identity.HostID, identity.SessionID)
		}
		seen[identity] = struct{}{}
	}
	return nil
}
