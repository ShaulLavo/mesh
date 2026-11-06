// Package htmlrender encodes the string contexts used by Mesh's two HTML pages.
package htmlrender

import (
	"encoding/json"
	"html"
	"strings"
)

var textExtras = strings.NewReplacer("\x00", "\ufffd", "+", "&#43;")

// Text encodes HTML text and double-quoted attributes with the original template spelling.
func Text(value string) string {
	return textExtras.Replace(html.EscapeString(value))
}

// Href filters whole URLs before URL normalization and HTML attribute encoding.
func Href(value string) string {
	scheme, _, hasScheme := strings.Cut(value, ":")
	if hasScheme && !strings.Contains(scheme, "/") && !strings.EqualFold(scheme, "https") && !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "mailto") {
		return "#ZgotmplZ"
	}
	return Text(urlPart(value, true))
}

// Query encodes one query value, not the surrounding URL or its delimiters.
func Query(value string) string {
	return urlPart(value, false)
}

// String encodes a string expression in an inline script, including HTML and line separators.
func String(value string) string {
	encoded, _ := json.Marshal(value) // A plain string cannot fail JSON encoding.
	return string(encoded)
}

// Whole URLs retain valid percent escapes; query values encode every percent byte.
func urlPart(value string, whole bool) string {
	const hex = "0123456789abcdef"
	var result strings.Builder
	written := 0
	for index := 0; index < len(value); index++ {
		if urlByteAllowed(value, index, whole) {
			continue
		}
		result.WriteString(value[written:index])
		result.WriteByte('%')
		result.WriteByte(hex[value[index]>>4])
		result.WriteByte(hex[value[index]&15])
		written = index + 1
	}
	if written == 0 {
		return value
	}
	result.WriteString(value[written:])
	return result.String()
}

func urlByteAllowed(value string, index int, whole bool) bool {
	c := value[index]
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
		return true
	}
	if !whole {
		return false
	}
	if strings.ContainsRune("!#$&*+,/:;=?@[]", rune(c)) {
		return true
	}
	return c == '%' && index+2 < len(value) && isHex(value[index+1]) && isHex(value[index+2])
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
