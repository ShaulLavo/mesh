package mcphost

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/shaul/mesh/internal/mcpwire"
)

// idPrefix marks identifiers this daemon run assigned. A reply carrying the
// prefix of an earlier run is a late reply from before a restart, never a
// reply to a current request.
const idPrefix = "mesh-"

// hostIDs assigns request IDs and progress tokens unique to one daemon run.
// The nonce changes on every start, so an adopted worker's replies to the
// previous daemon cannot be confused with replies to new requests.
type hostIDs struct {
	nonce   string
	counter atomic.Uint64
}

func newHostIDs() (*hostIDs, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("mcp host: create request ID nonce: %w", err)
	}
	return &hostIDs{nonce: hex.EncodeToString(nonce[:])}, nil
}

func (h *hostIDs) next(kind byte) (mcpwire.ID, string) {
	value := idPrefix + h.nonce + "-" + string(kind) + strconv.FormatUint(h.counter.Add(1), 36)
	raw, _ := json.Marshal(value)
	id, _ := mcpwire.ParseID(raw)
	return id, value
}

func (h *hostIDs) request() (mcpwire.ID, string) { return h.next('r') }

func (h *hostIDs) progress() string {
	_, value := h.next('p')
	return value
}

// ownership classifies a string identifier read from an upstream.
type ownership uint8

const (
	foreignID ownership = iota
	currentRunID
	earlierRunID
)

func (h *hostIDs) owner(value string) ownership {
	if !strings.HasPrefix(value, idPrefix) {
		return foreignID
	}
	if strings.HasPrefix(value, idPrefix+h.nonce+"-") {
		return currentRunID
	}
	return earlierRunID
}
