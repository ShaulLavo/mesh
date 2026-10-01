package release

import "testing"

// Fresh identity readers measure cold image hashing; Current's process cache
// would hide that work.
func BenchmarkExecutingBuildRead(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		build := newExecutingBuild(executingExecutablePath())()
		if build.Digest == "" {
			b.Fatal("executable digest missing")
		}
	}
}
