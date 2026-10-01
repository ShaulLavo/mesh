package cli

import "testing"

func BenchmarkCommandConstruction(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		command := NewCommand(Dependencies{})
		if len(command.Commands()) == 0 {
			b.Fatal("command tree is empty")
		}
	}
}
