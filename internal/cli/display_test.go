package cli

import (
	"strings"
	"testing"
)

func TestSafeTerminalTextPreservesEmojiJoinersAndEscapesBidiControls(t *testing.T) {
	const joinedText = "🤸‍♂️✨ A\u200cB"
	got := SafeTerminalText(joinedText + "\u202ereversed")

	if !strings.HasPrefix(got, joinedText) {
		t.Fatalf("safe terminal text = %q, want prefix %q", got, joinedText)
	}
	if strings.ContainsRune(got, '\u202e') || !strings.Contains(got, `\u202e`) {
		t.Fatalf("safe terminal text did not escape bidi override: %q", got)
	}
}
