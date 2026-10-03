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
	"net/url"
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

const (
	kindHost          = "host"
	kindPath          = "path"
	kindURL           = "url"
	kindUUID          = "uuid"
	kindUserHost      = "user-host"
	uuidPattern       = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	ipPattern         = `\[[0-9a-f:.]+(?:%[a-z0-9_.-]+)?\]|[0-9a-f]*:[0-9a-f:.]*(?:%[a-z0-9_.-]+)?|(?:[0-9]{1,3}\.){3}[0-9]{1,3}`
	accountPattern    = `[a-z0-9.!#$%&'*+=?^_` + "`" + `{|}~-]+@(?:\[[0-9a-f:.]+(?:%[a-z0-9_.-]+)?\]|[0-9a-f]*:[0-9a-f:.]+(?:%[a-z0-9_.-]+)?|[a-z0-9_][a-z0-9_.-]*)`
	privateDNSPattern = `[a-z0-9_-]+(?:\.[a-z0-9_-]+)*\.mesh\.(?:[a-z0-9-]+\.)+[a-z]{2,}(?::[0-9]+)?(?:/[^\s<>"'\x00-\x20]*)?`
)

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

// Value preserves readable host/session/title/service/name identities while
// applying Text to recognizable sensitive fragments. App, label, route, fleet,
// and notice fields use the same policy. Error and context remain opaque because
// arbitrary diagnostics can contain secrets that Text cannot recognize. Paths
// abbreviate /home/<user> and /Users/<user> to ~ while retaining useful suffixes
// and non-home paths; account, UUID, and IP fragments within paths are masked.
// URLs retain their scheme and path with private authorities sanitized.
// All other kinds, including account/owner/email/user/username/handle and opaque
// identifiers, become kind-<6 hex>. Kind is a public caller classification,
// never private data. Empty values remain empty.
func (m *Mask) Value(kind, value string) string {
	if m == nil || value == "" {
		return value
	}
	switch kind {
	case kindHost, "session", "title", "service", "name", "app", "label", "route", "fleet", "notice":
		return m.Text(value)
	case kindPath:
		return m.scrubIdentifiers(readablePath(value))
	case kindURL:
		return m.readableURL(value)
	default:
		return m.alias(kind, value)
	}
}

// Separate alias creation from policy dispatch so Text never recurses into Value.
func (m *Mask) alias(kind, value string) string {
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
	`|` + privateDNSPattern +
	`|` + accountPattern +
	`|(?:~[/\\]|[a-z]:[/\\]|/)[^\s<>"'\x00-\x20]+` +
	`|(?:[a-z0-9_-]+\.)+(?:ts\.net|tailscale\.net)\.?` +
	`|` + uuidPattern + `|` + ipPattern)

var privateDNS = regexp.MustCompile(`(?i)^` + privateDNSPattern + `$`)
var identifiers = regexp.MustCompile(`(?i)\x1b\[[0-?]*[ -/]*[@-~]|` + accountPattern + `|` + uuidPattern + `|` + ipPattern)
var literalIdentifiers = regexp.MustCompile(`(?i)\x1b\[[0-?]*[ -/]*[@-~]|` + uuidPattern + `|` + ipPattern)

// Text aliases email accounts, UUIDs, IP literals, and usernames in user@host
// forms while retaining ordinary host names. Tailnet names become
// hostname.<tailnet>; URLs retain scheme/port/path but hide DNS authorities,
// userinfo, queries, and fragments. Standalone *.mesh.<domain> routes hide
// their authority too. Home paths abbreviate the OS username to ~; UUID and IP
// and account fragments inside paths and URL paths are masked without
// re-matching paths.
// Surrounding text and ANSI escapes survive. Arbitrary secrets may not.
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
		return m.maskMatch(kind, value) + match[len(value):]
	})
}

func sensitiveKind(value string) string {
	switch {
	case strings.Contains(value, "://"), privateDNS.MatchString(value):
		return kindURL
	case absoluteOrHomePath(value):
		return kindPath
	case strings.Contains(value, "@"):
		return accountFormKind(value)
	case strings.HasSuffix(strings.ToLower(value), ".ts.net"), strings.HasSuffix(strings.ToLower(value), ".tailscale.net"):
		return kindHost
	case len(value) == 36 && value[8] == '-' && value[13] == '-':
		return kindUUID
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

func accountFormKind(value string) string {
	_, host, _ := strings.Cut(value, "@")
	if strings.Contains(host, ".") && tailnetHost(host) == host {
		return "email"
	}
	return kindUserHost
}

func (m *Mask) maskMatch(kind, value string) string {
	switch kind {
	case kindPath:
		return m.scrubIdentifiers(readablePath(value))
	case kindHost:
		return m.scrubLiterals(tailnetHost(value))
	case kindURL:
		return m.readableURL(value)
	case kindUserHost:
		user, host, _ := strings.Cut(value, "@")
		return m.alias("username", user) + "@" + m.scrubLiterals(tailnetHost(host))
	default:
		return m.alias(kind, value)
	}
}

func readablePath(value string) string {
	for _, prefix := range []string{"/home/", "/Users/"} {
		if rest, ok := strings.CutPrefix(value, prefix); ok && rest != "" {
			_, suffix, found := strings.Cut(rest, "/")
			if found {
				return "~/" + suffix
			}
			return "~"
		}
	}
	return value
}

func tailnetHost(value string) string {
	lower := strings.ToLower(value)
	if strings.HasSuffix(lower, ".ts.net") || strings.HasSuffix(lower, ".tailscale.net") {
		host, _, _ := strings.Cut(value, ".")
		return host + ".<tailnet>"
	}
	return value
}

// Retained paths use a restricted matcher without path or URL alternatives.
// Account host handling uses a still narrower literal matcher, so replacements
// cannot feed back into the same regex or recursively consume the path.
func (m *Mask) scrubIdentifiers(value string) string {
	return identifiers.ReplaceAllStringFunc(value, func(fragment string) string {
		kind := sensitiveKind(fragment)
		switch kind {
		case kindUUID, "ip", "email", kindUserHost:
			return m.maskMatch(kind, fragment)
		default:
			return fragment
		}
	})
}

func (m *Mask) scrubLiterals(value string) string {
	return literalIdentifiers.ReplaceAllStringFunc(value, func(fragment string) string {
		kind := sensitiveKind(fragment)
		if kind == kindUUID || kind == "ip" {
			return m.alias(kind, fragment)
		}
		return fragment
	})
}

func (m *Mask) urlPath(escaped string) string {
	segments := strings.Split(escaped, "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			continue
		}
		if masked := m.scrubIdentifiers(decoded); masked != decoded {
			segments[i] = url.PathEscape(masked)
		}
	}
	return strings.Join(segments, "/")
}

func (m *Mask) readableURL(value string) string {
	parseValue := value
	prefix := ""
	if !strings.Contains(value, "://") {
		parseValue = "//" + value
	}
	u, err := url.Parse(parseValue)
	if err != nil || u.Host == "" {
		return m.alias(kindURL, value)
	}
	if u.Scheme != "" {
		prefix = u.Scheme + "://"
	}
	host := m.urlHost(u.Hostname())
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	return prefix + host + m.urlPath(u.EscapedPath())
}

func (m *Mask) urlHost(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return m.alias("ip", host)
	}
	if masked := tailnetHost(host); masked != host {
		return m.scrubLiterals(masked)
	}
	return "<domain>"
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
