package mcpgateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

var errBadCursor = errors.New("invalid pagination cursor")

// pageCursor binds a page offset to the exact listing it came from. A
// changed profile, identity, kind or listing makes it fail, and the client
// lists again from the start.
type pageCursor struct {
	Version int    `json:"v"`
	Profile string `json:"p"`
	// Selection digests the profile entry, so a profile change fails the
	// cursor while an hx_ allocation elsewhere in the file does not.
	Selection string `json:"r"`
	Method    string `json:"m"`
	Snapshot  string `json:"s"`
	Offset    int    `json:"o"`
}

type cursorSigner struct {
	key []byte
}

// newCursorSigner derives the signing key from the frontend's private
// application credential, so a fresh frontend with the same identity accepts
// cursors of an earlier one. Without one it uses a random process key.
func newCursorSigner(secret []byte) cursorSigner {
	if len(secret) == 0 {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		return cursorSigner{key: key}
	}
	mac := hmac.New(sha256.New, []byte("mesh-mcp-gateway-cursor-v1"))
	mac.Write(secret)
	return cursorSigner{key: mac.Sum(nil)}
}

func (s cursorSigner) sign(c pageCursor) string {
	payload, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body))
}

func (s cursorSigner) mac(body string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

func (s cursorSigner) verify(token string) (pageCursor, error) {
	body, signature, ok := strings.Cut(token, ".")
	if !ok || len(token) > 1024 {
		return pageCursor{}, errBadCursor
	}
	want, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(want, s.mac(body)) {
		return pageCursor{}, errBadCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return pageCursor{}, errBadCursor
	}
	var c pageCursor
	if json.Unmarshal(payload, &c) != nil || c.Version != 1 || c.Offset <= 0 {
		return pageCursor{}, errBadCursor
	}
	return c, nil
}
