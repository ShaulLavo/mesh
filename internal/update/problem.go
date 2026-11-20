package update

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ProblemCode string

type controlProblem struct {
	code ProblemCode
	text string
}

func (p *controlProblem) Error() string           { return p.text }
func problem(code ProblemCode, text string) error { return &controlProblem{code: code, text: text} }
func responseProblemCode(err error) ProblemCode {
	var specific *controlProblem
	if errors.As(err, &specific) {
		return specific.code
	}
	return ""
}

func boundedProblem(text string) string {
	text = strings.Map(plainProblemRune, text)
	if len(text) <= 512 {
		return text
	}
	text = text[:512]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
func plainProblemRune(value rune) rune {
	if unicode.IsControl(value) {
		return ' '
	}
	return value
}

type VerifiedActor struct {
	id    string
	local bool
}

func (a VerifiedActor) ID() string { return a.id }

func newAutomaticID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("update: allocate authority identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func NewAutomaticCommandID() (string, error) { return newAutomaticID() }

func DecodeAutomatic(data json.RawMessage, value any) error { return decodeAuthority(data, value) }
