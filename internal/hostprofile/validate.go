package hostprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"unicode/utf8"
)

var labelKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const labelsField = "labels"
const warningsField = "warnings"

var editableFields = []string{"schemaVersion", "description", labelsField, warningsField, "instructions"}

func ParseEditable(data []byte, limits Limits) (Editable, error) {
	var p Editable
	if err := strictObject(data, limits.Encoded, editableFields); err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, invalid("document", err.Error())
	}
	return normalizeEditable(p, limits)
}

func invalid(field, problem string) error { return &ValidationError{Field: field, Problem: problem} }

func strictObject(data []byte, maximum int, fields []string) error {
	if len(data) > maximum {
		return invalid("document", fmt.Sprintf("encoded size exceeds %d bytes", maximum))
	}
	if !utf8.Valid(data) {
		return invalid("document", "must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := CheckJSON(decoder, 0, 8); err != nil {
		return invalid("document", err.Error())
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid("document", "trailing data")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return invalid("document", "must be an object")
	}
	for key, value := range object {
		if err := checkField(key, value, fields); err != nil {
			return err
		}
	}
	// encoding/json replaces lone surrogate escapes even in otherwise valid UTF-8.
	if err := checkSurrogates(data); err != nil {
		return err
	}
	return nil
}

func checkField(key string, value json.RawMessage, fields []string) error {
	allowed := false
	for _, field := range fields {
		if field == key {
			allowed = true
			break
		}
	}
	if !allowed {
		return invalid(key, "unknown field")
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && key != labelsField && key != warningsField {
		return invalid(key, "must not be null")
	}
	if key == labelsField || key == warningsField {
		return rejectNullItems(key, value)
	}
	return nil
}

func rejectNullItems(key string, value json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil
	}
	var items []json.RawMessage
	if key == warningsField {
		if err := json.Unmarshal(value, &items); err != nil {
			return invalid(key, err.Error())
		}
	}
	if key == labelsField {
		var labels map[string]json.RawMessage
		if err := json.Unmarshal(value, &labels); err != nil {
			return invalid(key, err.Error())
		}
		for _, v := range labels {
			items = append(items, v)
		}
	}
	for _, item := range items {
		if bytes.Equal(bytes.TrimSpace(item), []byte("null")) {
			return invalid(key, "entries must be strings, not null")
		}
	}
	return nil
}

func normalizeEditable(p Editable, limits Limits) (Editable, error) {
	if p.SchemaVersion != SchemaVersion {
		return p, invalid("schemaVersion", "must be 1")
	}
	if err := validateText("description", p.Description, limits.Description, false); err != nil {
		return p, err
	}
	if err := validateText("instructions", p.Instructions, limits.Instructions, true); err != nil {
		return p, err
	}
	if len(p.Labels) > limits.Labels {
		return p, invalid(labelsField, fmt.Sprintf("exceeds %d entries", limits.Labels))
	}
	if len(p.Warnings) > limits.Warnings {
		return p, invalid(warningsField, fmt.Sprintf("exceeds %d entries", limits.Warnings))
	}
	if err := validateLabels(p.Labels, limits); err != nil {
		return p, err
	}
	for _, warning := range p.Warnings {
		if err := validateText("warning", warning, limits.Warning, false); err != nil {
			return p, err
		}
	}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return p, invalid("document", err.Error())
	}
	if len(encoded) > limits.Encoded {
		return p, invalid("document", fmt.Sprintf("encoded size exceeds %d bytes", limits.Encoded))
	}
	return p, nil
}

func validateLabels(labels map[string]string, limits Limits) error {
	for key, value := range labels {
		if len(key) > limits.LabelKey || !labelKey.MatchString(key) {
			return invalid("label key", fmt.Sprintf("must match [a-z0-9][a-z0-9._-]* and fit %d bytes", limits.LabelKey))
		}
		if err := validateText("label value", value, limits.LabelValue, false); err != nil {
			return err
		}
	}
	return nil
}

func validateText(field, value string, maximum int, multiline bool) error {
	if !utf8.ValidString(value) {
		return invalid(field, "must be valid UTF-8")
	}
	if len(value) > maximum {
		return invalid(field, fmt.Sprintf("exceeds %d bytes", maximum))
	}
	for _, r := range value {
		if forbiddenControl(r, multiline) {
			return invalid(field, "contains a disallowed control character")
		}
	}
	return nil
}

func forbiddenControl(r rune, multiline bool) bool {
	if multiline && (r == '\n' || r == '\t') {
		return false
	}
	return r < 0x20 || r >= 0x7f && r <= 0x9f || r == 0x2028 || r == 0x2029 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

func checkSurrogates(data []byte) error {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' {
			continue
		}
		value, ok := escapedHex(data, i+1)
		if !ok {
			continue
		}
		i += 4
		if value < 0xd800 || value > 0xdfff {
			continue
		}
		if err := checkSurrogatePair(data, i, value); err != nil {
			return err
		}

		i += 6
	}
	return nil
}

func checkSurrogatePair(data []byte, index int, high uint16) error {
	if high > 0xdbff || index+6 >= len(data) || string(data[index+1:index+3]) != "\\u" {
		return invalid("document", "unpaired Unicode surrogate")
	}
	low, ok := escapedHex(data, index+3)
	if !ok || low < 0xdc00 || low > 0xdfff {
		return invalid("document", "unpaired Unicode surrogate")
	}
	return nil
}

func escapedHex(data []byte, index int) (uint16, bool) {
	if index+4 > len(data) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(data[index:index+4]), 16, 16)
	return uint16(value), err == nil
}
