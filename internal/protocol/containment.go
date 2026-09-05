package protocol

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/session"
)

const (
	// MaxContainingSessions bounds one complete terminal-containment path,
	// including the worker that answers a containment query.
	MaxContainingSessions = 32
	// MaxSessionIdentityHostIDBytes bounds opaque host identities received from
	// a peer. Mesh's Ed25519 host IDs are currently 43 bytes.
	MaxSessionIdentityHostIDBytes = 256
)

// SessionIdentity names one exact session on one exact host. Session IDs are
// host-local, so neither field identifies a terminal on its own.
type SessionIdentity struct {
	HostID    string `json:"hostId"`
	SessionID string `json:"sessionId"`
}

// ValidateSessionIdentity validates an identity received across a protocol
// boundary. Exact identities use canonical values rather than normalizing a
// peer's ambiguous input.
func ValidateSessionIdentity(identity SessionIdentity) error {
	if identity.HostID == "" || strings.TrimSpace(identity.HostID) != identity.HostID {
		return fmt.Errorf("protocol: session identity has an empty or non-canonical host ID")
	}
	if len(identity.HostID) > MaxSessionIdentityHostIDBytes {
		return fmt.Errorf(
			"protocol: session identity host ID is %d bytes; maximum is %d",
			len(identity.HostID),
			MaxSessionIdentityHostIDBytes,
		)
	}
	if !utf8.ValidString(identity.HostID) {
		return fmt.Errorf("protocol: session identity host ID is not valid UTF-8")
	}
	for _, character := range identity.HostID {
		if unicode.IsControl(character) {
			return fmt.Errorf("protocol: session identity host ID contains a control character")
		}
	}

	parsed, err := session.ParseID(identity.SessionID)
	if err != nil {
		return fmt.Errorf("protocol: session identity: %w", err)
	}
	if parsed != identity.SessionID {
		return fmt.Errorf("protocol: session identity has non-canonical session ID %q", identity.SessionID)
	}
	return nil
}

// ValidateContainingSessions checks one immediate-to-outer containment path. A
// repeated exact session would make the path cyclic and is rejected.
func ValidateContainingSessions(identities []SessionIdentity) error {
	if len(identities) > MaxContainingSessions {
		return fmt.Errorf(
			"protocol: containment has %d sessions; maximum is %d",
			len(identities),
			MaxContainingSessions,
		)
	}
	seen := make(map[SessionIdentity]struct{}, len(identities))
	for index, identity := range identities {
		if err := ValidateSessionIdentity(identity); err != nil {
			return fmt.Errorf("protocol: containing session %d: %w", index, err)
		}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf(
				"protocol: containing session %d duplicates %s/%s",
				index,
				identity.HostID,
				identity.SessionID,
			)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

// CloneSessionIdentities returns storage independent of the source slice.
func CloneSessionIdentities(source []SessionIdentity) []SessionIdentity {
	return append([]SessionIdentity(nil), source...)
}
