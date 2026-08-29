package session

import "testing"

func TestParseIDNormalizesAndValidatesCrockfordBase32(t *testing.T) {
	for _, input := range []string{"7K3D", "7k3d"} {
		got, err := ParseID(input)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", input, err)
		}
		if got != "7K3D" {
			t.Fatalf("ParseID(%q) = %q, want 7K3D", input, got)
		}
	}

	for _, input := range []string{"", "ABC", "ABCDE", "AB/D", "ABID", "ABLD", "ABOD", "ABUD"} {
		if _, err := ParseID(input); err == nil {
			t.Errorf("ParseID(%q) accepted an invalid session ID", input)
		}
	}
}
