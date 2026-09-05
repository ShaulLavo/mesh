//go:build darwin

package worker

import "testing"

func TestDarwinTerminalDeviceFromLsof(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "terminal", output: "p123\nf0\nn/dev/ttys004\n", want: "/dev/ttys004"},
		{name: "not terminal", output: "p123\nf0\nn/dev/null\n"},
		{name: "relative", output: "p123\nf0\nnttys004\n"},
		{name: "ambiguous", output: "p123\nf0\nn/dev/ttys004\nn/dev/ttys005\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := darwinTerminalDeviceFromLsof(test.output); got != test.want {
				t.Fatalf("darwinTerminalDeviceFromLsof() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDarwinPTYSlavePath(t *testing.T) {
	for _, path := range []string{"/dev/ttys000", "/dev/ttys00a", "/dev/ttysABC"} {
		if !darwinPTYSlavePath(path) {
			t.Fatalf("darwinPTYSlavePath(%q) = false", path)
		}
	}
	for _, path := range []string{
		"", "dev/ttys001", "/dev/ttys", "/dev/ttys/001", "/dev/ttys00-",
		"/dev/tty", "/dev/null", "/tmp/ttys001", "/dev/ttys001 (deleted)",
	} {
		if darwinPTYSlavePath(path) {
			t.Fatalf("darwinPTYSlavePath(%q) = true", path)
		}
	}
}
