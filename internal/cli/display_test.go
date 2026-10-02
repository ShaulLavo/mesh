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

func TestSafeTerminalTextPreservesCommandQuotesAcrossPresentation(t *testing.T) {
	const command = `sh -c 'cd -- "$1" && exec "${SHELL:-/bin/bash}" -l' mesh-workspace /home/user/Projects`
	got := command
	for range 3 {
		got = SafeTerminalText(got)
		if got != command {
			t.Fatalf("command text = %q, want %q", got, command)
		}
	}
}

func TestSafeTerminalTextEscapesControlsWithoutReescapingVisibleText(t *testing.T) {
	const input = "quoted \"text\" \\path\x1b[2J\n\t\u202e"
	const want = `quoted "text" \path\x1b[2J\n\t\u202e`
	got := SafeTerminalText(input)
	if got != want {
		t.Fatalf("safe text = %q, want %q", got, want)
	}
	if repeated := SafeTerminalText(got); repeated != got {
		t.Fatalf("presenting safe text again changed it: %q -> %q", got, repeated)
	}
}
