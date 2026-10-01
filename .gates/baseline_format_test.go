package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCommittedBaselineUsesCanonicalFormat(t *testing.T) {
	entries, err := readBaseline("baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeBaseline(path, entries); err != nil {
		t.Fatal(err)
	}
	if bytesAt(t, path) != bytesAt(t, "baseline.json") {
		t.Fatal("committed baseline is not in the updater's canonical format")
	}
}

func TestStaleRemovalChangesOnlyItsCanonicalLine(t *testing.T) {
	entries := map[findingKey]entry{sampleKey(): sampleEntry(1)}
	second := sampleEntry(1)
	second.File = "second.go"
	entries[second.findingKey] = second
	key := sortedKeys(entries)[0]
	actual := make(map[findingKey]int, len(entries))
	for candidate, item := range entries {
		if candidate != key {
			actual[candidate] = item.Count
		}
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := writeBaseline(path, entries); err != nil {
		t.Fatal(err)
	}
	before := bytesAt(t, path)
	if err := reduceBaseline(path, entries, actual, map[findingKey]int{key: entries[key].Count}); err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(before, "\n")
	want := strings.Join(append(lines[:3], lines[4:]...), "")
	if bytesAt(t, path) != want {
		t.Fatal("one stale removal changed more than its canonical baseline line")
	}
}
