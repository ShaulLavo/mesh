package release

import "testing"

// Package init precedes main's CPU profile. Re-running exactly that read here
// attributes CLI startup without changing the shipped initialization path.
func BenchmarkExecutingBuildRead(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		build := readExecutingBuild()
		if build.Digest == "" {
			b.Fatal("executable digest missing")
		}
	}
}
