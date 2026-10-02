// Package privacy provides opt-in presentation masking for known metadata and
// free-form errors. Callers choose a *Mask explicitly; a nil mask preserves the
// original data. There is no mutable global enable switch.
//
// Text is best-effort recognition, not a security boundary. Arbitrary logs,
// terminal/screen contents, secrets, and unstructured payloads cannot be made
// safe with regular expressions and must be withheld by callers. Prefer Value
// for fields whose meaning is known, and Command for command summaries.
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"os"
	"path"
	"regexp"
	"strings"
)

// Mask is an opt-in presentation policy. Its zero value is ready to use and it
// is safe for concurrent use. All masks share a random process-lifetime key so
// an alias stays stable across frames and independently constructed masks.
// Aliases are short labels, not unique identifiers or persistent pseudonyms.
// Restarting the process changes them.
type Mask struct{}

var processKey = func() [32]byte {
	var key [32]byte
	// Continuing without entropy would make private values guessable offline.
	if _, err := rand.Read(key[:]); err != nil {
		panic("privacy: generate process key: " + err.Error())
	}
	return key
}()

// New enables masking for the caller that retains the returned policy.
func New() *Mask { return &Mask{} }

// Value returns kind-<6 hex> for a nonempty value. Kind is a public,
// caller-provided classification (such as "host"), never private data. The same
// kind/value pair has the same alias for the process lifetime. Different kinds
// have separate alias namespaces. Empty values remain empty.
func (m *Mask) Value(kind, value string) string {
	if m == nil || value == "" {
		return value
	}
	h := hmac.New(sha256.New, processKey[:])
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return kind + "-" + hex.EncodeToString(h.Sum(nil)[:3])
}

// Keep alternatives in one expression so replacements are never matched again.
// More specific forms precede embedded paths, host names, and address literals.
var sensitive = regexp.MustCompile(`(?i)` +
	`\x1b\[[0-?]*[ -/]*[@-~]` +
	`|[a-z][a-z0-9+.-]*://[^\s<>"'\x00-\x20]+` +
	`|[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9_][a-z0-9_.-]*` +
	`|(?:~[/\\]|[a-z]:[/\\]|/)[^\s<>"'\x00-\x20]+` +
	`|(?:[a-z0-9_-]+\.)+(?:ts\.net|tailscale\.net)\.?` +
	`|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}` +
	`|\[[0-9a-f:.]+(?:%[a-z0-9_.-]+)?\]` +
	`|[0-9a-f]*:[0-9a-f:.]*(?:%[a-z0-9_.-]+)?` +
	`|(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)

// Text replaces recognized emails, URLs, absolute/home paths, tailnet DNS
// names, UUIDs, user@host forms, and IP literals. It preserves surrounding text
// and ANSI escapes, but cannot detect every spelling or sensitive value.
func (m *Mask) Text(text string) string {
	if m == nil {
		return text
	}
	return sensitive.ReplaceAllStringFunc(text, func(match string) string {
		if strings.HasPrefix(match, "\x1b[") {
			return match
		}
		value := strings.TrimRight(match, ".,;!?) }")
		if value == "" {
			return match
		}
		kind := sensitiveKind(value)
		if kind == "" {
			return match
		}
		return m.Value(kind, value) + match[len(value):]
	})
}

func sensitiveKind(value string) string {
	switch {
	case strings.Contains(value, "://"):
		return "url"
	case strings.Contains(value, "@"):
		if strings.Contains(strings.SplitN(value, "@", 2)[1], ".") {
			return "email"
		}
		return "user-host"
	case absoluteOrHomePath(value):
		return "path"
	case strings.HasSuffix(strings.ToLower(value), ".ts.net"), strings.HasSuffix(strings.ToLower(value), ".tailscale.net"):
		return "host"
	case len(value) == 36 && value[8] == '-' && value[13] == '-':
		return "uuid"
	default:
		if _, err := netip.ParseAddr(strings.Trim(value, "[]")); err == nil {
			return "ip"
		}
		return ""
	}
}

func absoluteOrHomePath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return true
	}
	return len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

// Command retains only the executable basename and, when arguments exist, a
// single "[arguments withheld]" marker. It does not mutate argv. Executable
// names are deliberately public in this summary; callers must withhold the
// entire command if even its basename is sensitive. A nil mask returns argv
// unchanged, including its backing slice.
func (m *Mask) Command(argv []string) []string {
	if m == nil || len(argv) == 0 {
		return argv
	}
	result := []string{path.Base(strings.ReplaceAll(argv[0], `\`, "/"))}
	if argv[0] == "" {
		result[0] = ""
	}
	if len(argv) > 1 {
		result = append(result, "[arguments withheld]")
	}
	return result
}

// EnabledFromEnv reads MESH_PRIVACY on each call. Only 1, true, yes, and on
// (case-insensitive, with surrounding whitespace ignored) enable masking.
// Callers use this decision to choose New() or nil; it changes no policy.
func EnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MESH_PRIVACY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
