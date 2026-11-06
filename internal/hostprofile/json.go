package hostprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// CheckJSON rejects duplicate and Unicode case-ambiguous keys before decoding.
// The caller retains control of byte limits, trailing data and field schemas.
func CheckJSON(decoder *json.Decoder, depth, maximum int) error {
	if depth > maximum {
		return fmt.Errorf("configuration nesting exceeds %d levels", maximum)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read configuration token: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if err := checkCompositeKey(decoder, delimiter, keys); err != nil {
			return err
		}
		if err := CheckJSON(decoder, depth+1, maximum); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close configuration object: %w", err)
	}
	return nil
}

func checkCompositeKey(decoder *json.Decoder, delimiter json.Delim, keys map[string]bool) error {
	if delimiter != '{' {
		return nil
	}
	return checkJSONKey(decoder, keys)
}

func checkJSONKey(decoder *json.Decoder, keys map[string]bool) error {
	key, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read configuration key: %w", err)
	}
	name, ok := key.(string)
	if !ok {
		return errors.New("configuration object key must be a string")
	}
	folded := foldJSONKey(name)
	if keys[folded] {
		return errors.New("configuration contains duplicate or case-ambiguous object keys")
	}
	keys[folded] = true
	return nil
}

// foldJSONKey follows encoding/json's Unicode simple-fold field matching.
func foldJSONKey(name string) string { return strings.Map(foldJSONRune, name) }
func foldJSONRune(r rune) rune {
	folded := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		if next < folded {
			folded = next
		}
	}
	return folded
}
