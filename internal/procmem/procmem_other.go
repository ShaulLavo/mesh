//go:build !linux && !darwin

package procmem

// Snapshot measures nothing where no process table reader exists.
func Snapshot() Table { return Table{} }

func commandLine(int) []string { return nil }
