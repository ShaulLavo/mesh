package sshfs_test

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSCPRecursiveDownloadPreservesSiblingDirectories(t *testing.T) {
	f := newFixture(t)
	for _, directory := range []string{"a", "ab", "ab/nested", "abc"} {
		local := filepath.Join(f.root, "prefixes", directory)
		if err := os.MkdirAll(local, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(local, "value.txt"), "belongs to "+directory)
	}
	connection := newSSHClient(t, f.registry)
	session, reader, input := startSCPDownload(t, connection, "scp -r -f /files/prefixes")
	assertSCPHeader(t, reader, input, "D0500 0 prefixes\n")
	assertSCPHeader(t, reader, input, "D0500 0 a\n")
	assertSCPFileRecord(t, reader, input, "value.txt", "belongs to a")
	assertSCPHeader(t, reader, input, "E\n")
	assertSCPHeader(t, reader, input, "D0500 0 ab\n")
	assertSCPHeader(t, reader, input, "D0500 0 nested\n")
	assertSCPFileRecord(t, reader, input, "value.txt", "belongs to ab/nested")
	assertSCPHeader(t, reader, input, "E\n")
	assertSCPFileRecord(t, reader, input, "value.txt", "belongs to ab")
	assertSCPHeader(t, reader, input, "E\n")
	assertSCPHeader(t, reader, input, "D0500 0 abc\n")
	assertSCPFileRecord(t, reader, input, "value.txt", "belongs to abc")
	assertSCPHeader(t, reader, input, "E\n")
	assertSCPHeader(t, reader, input, "E\n")
	if remaining, err := io.ReadAll(reader); err != nil || len(remaining) != 0 {
		t.Fatalf("trailing SCP records = %q, %v", remaining, err)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("recursive SCP download: %v", err)
	}
}

func assertSCPHeader(t *testing.T, reader *bufio.Reader, input io.Writer, want string) {
	t.Helper()
	if actual := readSCPHeader(t, reader, input); actual != want {
		t.Fatalf("SCP header = %q; want %q", actual, want)
	}
	assertSCPAcknowledge(t, input)
}
