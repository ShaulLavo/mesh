package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFailedSigningNeverWritesEnvelope(t *testing.T) {
	root := t.TempDir()
	bundle, subject, output := filepath.Join(root, "bundle"), filepath.Join(root, "subject"), filepath.Join(root, "output")
	for path, contents := range map[string]string{bundle: `{"mediaType":"unsupported"}`, subject: "artifact", output: "prior verified output"} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"wrap", bundle, output, subject}); err == nil {
		t.Fatal("accepted unsupported bundle")
	}
	contents, err := os.ReadFile(output) //nolint:gosec // The test reads its own temporary output.
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "prior verified output" {
		t.Fatal("failed signing replaced output")
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"wrap", bundle, output, subject}); err == nil {
		t.Fatal("accepted unsupported bundle")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed signing created output: %v", err)
	}
}

func TestReleaseSignerRejectsMissingFilesAndArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"wrap"}, {"verify"}, {"metadata"}, {"verify-metadata"}, {"refresh"}, {"metadata", "missing"}, {"verify", "missing", "missing"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
