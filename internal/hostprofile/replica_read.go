package hostprofile

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

var FleetLimits = Limits{1 << 10, 64 << 10, 32, 64, 256, 16, 512, 96 << 10}

const StateUntrusted ProfileState = "untrusted"
const fleetRevisionField = "revision"
const fleetSignaturePrefix = "mesh-fleet-profile-v1\x00"

type FleetDocument struct {
	Editable
	Fleet     string    `json:"fleet"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    string    `json:"author"`
	Signature string    `json:"signature"`
}

func (d FleetDocument) signedBytes() []byte {
	d.Signature = ""
	data, _ := json.Marshal(d)
	return append([]byte(fleetSignaturePrefix), data...)
}

func (d FleetDocument) Digest() string {
	digest := sha256.Sum256(d.signedBytes())
	return hex.EncodeToString(digest[:])
}

func VerifyFleet(d FleetDocument) error {
	if _, err := normalizeEditable(d.Editable, FleetLimits); err != nil {
		return err
	}
	if err := validateText("fleet", d.Fleet, 256, false); err != nil {
		return err
	}
	if d.Revision == 0 || d.UpdatedAt.IsZero() {
		return invalid("metadata", "requires a positive revision and updatedAt")
	}
	public, err := base64.RawURLEncoding.DecodeString(d.Author)
	if err != nil || len(public) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(public) != d.Author {
		return invalid("author", "must be a Mesh Ed25519 identity")
	}
	signature, err := base64.RawURLEncoding.DecodeString(d.Signature)
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != d.Signature || !ed25519.Verify(public, d.signedBytes(), signature) {
		return invalid("signature", "invalid fleet signature")
	}
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode fleet profile: %w", err)
	}
	if len(data) > FleetLimits.Encoded {
		return invalid("document", "stored fleet profile exceeds 98304 bytes")
	}
	return nil
}

func parseFleet(data []byte) (FleetDocument, error) {
	var doc FleetDocument
	fields := append(append([]string{}, editableFields...), "fleet", fleetRevisionField, "updatedAt", "author", "signature")
	if err := strictObject(data, FleetLimits.Encoded, fields); err != nil {
		return doc, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, invalid("document", err.Error())
	}
	if err := VerifyFleet(doc); err != nil {
		return doc, err
	}
	return doc, nil
}

func (s *FleetStore) ReadReplica(trusted func(string) bool) (Snapshot[FleetDocument], error) {
	revision := uint64(0)
	absent := Snapshot[FleetDocument]{State: StateAbsent, Revision: &revision}
	state, err := openDirectory(s.dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return absent, nil
	}
	if err != nil {
		return fleetUnavailable(err), err
	}
	defer func() { _ = state.Close() }()
	dir, err := openPrivateDirectoryChild(state, "fleet", false)
	if errors.Is(err, os.ErrNotExist) {
		return absent, nil
	}
	if err != nil {
		return fleetUnavailable(err), err
	}
	defer func() { _ = dir.Close() }()
	return readReplica(dir, trusted)
}

func readReplica(dir *os.File, trusted func(string) bool) (Snapshot[FleetDocument], error) {
	revision := uint64(0)
	absent := Snapshot[FleetDocument]{State: StateAbsent, Revision: &revision}
	data, err := readPrivate(dir, "profile.json", FleetLimits.Encoded)
	if errors.Is(err, os.ErrNotExist) {
		return absent, nil
	}
	if err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) {
			return Snapshot[FleetDocument]{State: StateInvalid, Problem: problemText(err.Error())}, nil
		}
		return fleetUnavailable(err), err
	}
	doc, err := parseFleet(data)
	if err != nil {
		return Snapshot[FleetDocument]{State: StateInvalid, Revision: fleetRevision(data), Problem: problemText(err.Error())}, nil
	}
	snapshot := Snapshot[FleetDocument]{State: StateValid, Value: &doc, Revision: &doc.Revision}
	if trusted == nil || !trusted(doc.Author) {
		snapshot.State, snapshot.Problem = StateUntrusted, "The fleet profile author is not a current update administrator on this machine."
	}
	return snapshot, nil
}

func fleetUnavailable(err error) Snapshot[FleetDocument] {
	return Snapshot[FleetDocument]{State: StateUnavailable, Problem: problemText(err.Error())}
}

func readPrivate(dir *os.File, name string, maximum int) ([]byte, error) {
	file, err := openPrivateFile(dir, name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maximum {
		return nil, invalid("document", fmt.Sprintf("encoded size exceeds %d bytes", maximum))
	}
	return data, nil
}

func openPrivateDirectoryChild(parent *os.File, name string, create bool) (*os.File, error) {
	dir, err := openDirectoryChild(parent, name, create)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("stat profile directory: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 { //nolint:gosec // Unix UIDs are unsigned 32-bit values.
		_ = dir.Close()
		return nil, errors.New("profile directory must be owned by this user and not writable by others")
	}
	return dir, nil
}

// Only strictly decoded metadata is revision evidence. A damaged signature never
// makes an older document safe to install, and malformed JSON is not absence.
func fleetRevision(data []byte) *uint64 {
	fields := append(append([]string{}, editableFields...), "fleet", fleetRevisionField, "updatedAt", "author", "signature")
	if strictObject(data, FleetLimits.Encoded, fields) != nil {
		return nil
	}
	var doc FleetDocument
	if json.Unmarshal(data, &doc) != nil || doc.Revision == 0 {
		return nil
	}
	return &doc.Revision
}
