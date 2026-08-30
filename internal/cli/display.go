package cli

import (
	"errors"
	"strconv"
	"strings"
)

const maximumTerminalTextBytes = 512

// SafeTerminalText escapes untrusted text and bounds one terminal cell. It is
// exported for the picker, which renders the same remote catalog as the CLI.
func SafeTerminalText(value string) string {
	value = strings.ToValidUTF8(value, "?")
	var output strings.Builder
	for _, character := range value {
		quoted := strconv.QuoteToGraphic(string(character))
		encoded := quoted[1 : len(quoted)-1]
		if output.Len()+len(encoded) > maximumTerminalTextBytes-len("...") {
			output.WriteString("...")
			break
		}
		output.WriteString(encoded)
	}
	return output.String()
}

type safePresentedError struct {
	prefix string
	cause  error
}

func (e safePresentedError) Error() string {
	detail := safeRemoteText(e.cause.Error())
	if detail == "" {
		return e.prefix
	}
	return e.prefix + ": " + detail
}

func (e safePresentedError) Unwrap() error { return e.cause }

func presentError(prefix string, cause error) error {
	if cause == nil {
		return errors.New(prefix)
	}
	return safePresentedError{prefix: prefix, cause: cause}
}
