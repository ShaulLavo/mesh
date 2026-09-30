package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleFile = "sample.go"
const sampleSource = "func sample() {}"

func sampleKey() findingKey {
	return findingKey{Gate: "golangci", File: sampleFile, Rule: "dupl", Text: "duplicated with sample.go:<location>", Source: sampleSource}
}

func sampleEntry(count int) entry {
	return entry{findingKey: sampleKey(), Count: count, Reason: "Existing clone; extraction is assigned to Phase 4."}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) options {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sampleFile), "package sample\n\n"+sampleSource+"\n")
	for _, gate := range gateNames {
		content := "[]"
		if gate == "golangci" {
			content = `{"Issues":[]}`
		}
		writeFile(t, filepath.Join(root, gate+".json"), content)
	}
	path := filepath.Join(root, "baseline.json")
	if err := writeBaseline(path, nil); err != nil {
		t.Fatal(err)
	}
	return options{root: root, reports: root, baseline: path}
}

func issueReport(t *testing.T, o options, file, text string, line, count int) {
	t.Helper()
	issues := make([]any, count)
	for i := range issues {
		issues[i] = map[string]any{"Pos": map[string]any{"Filename": file, "Line": line}, "FromLinter": "dupl", "Text": text}
	}
	data, err := json.Marshal(map[string]any{"Issues": issues})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(o.reports, "golangci.json"), string(data))
}

func expectCheck(t *testing.T, o options, want int) {
	t.Helper()
	status, err := check(o, io.Discard, io.Discard)
	if err != nil || status != want {
		t.Fatalf("check = %d, %v; want %d", status, err, want)
	}
}

func admit(t *testing.T, o options, count int) {
	t.Helper()
	if err := writeBaseline(o.baseline, map[findingKey]entry{sampleKey(): sampleEntry(count)}); err != nil {
		t.Fatal(err)
	}
}

func bytesAt(t *testing.T, path string) string {
	t.Helper()
	directory, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	data, err := directory.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewFindingFails(t *testing.T) {
	o := fixture(t)
	issueReport(t, o, sampleFile, "duplicated with sample.go:3-8", 3, 1)
	expectCheck(t, o, 1)
}

func TestUpdateRefusesGrowthWithoutPartialRemoval(t *testing.T) {
	o := fixture(t)
	admit(t, o, 1)
	issueReport(t, o, sampleFile, "new clone", 3, 1)
	before := bytesAt(t, o.baseline)
	o.update = true
	expectCheck(t, o, 1)
	if bytesAt(t, o.baseline) != before {
		t.Fatal("baseline changed despite refusal")
	}
}

func TestFixedEntryFailsUntilRemoved(t *testing.T) {
	o := fixture(t)
	admit(t, o, 1)
	expectCheck(t, o, 1)
	o.update = true
	expectCheck(t, o, 0)
	entries, err := readBaseline(o.baseline)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fixed entries remain: %v, %v", entries, err)
	}
}

func TestCountsOnlyDecrease(t *testing.T) {
	for _, actual := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(actual), func(t *testing.T) {
			o := fixture(t)
			admit(t, o, 2)
			issueReport(t, o, sampleFile, "duplicated with sample.go:3-8", 3, actual)
			o.update = true
			want := 0
			if actual > 2 {
				want = 1
			}
			expectCheck(t, o, want)
			entries, err := readBaseline(o.baseline)
			if err != nil {
				t.Fatal(err)
			}
			if entries[sampleKey()].Count != min(actual, 2) {
				t.Fatal("unexpected count after update")
			}
		})
	}
}

func TestNoopUpdateDoesNotRewrite(t *testing.T) {
	o := fixture(t)
	admit(t, o, 1)
	issueReport(t, o, sampleFile, "duplicated with sample.go:3-8", 3, 1)
	before := bytesAt(t, o.baseline)
	o.update = true
	expectCheck(t, o, 0)
	if bytesAt(t, o.baseline) != before {
		t.Fatal("no-op update rewrote the baseline")
	}
}

func TestLineMovementAndCloneRangesPreserveKeys(t *testing.T) {
	o := fixture(t)
	issueReport(t, o, sampleFile, "3-8 lines are duplicate of sample.go:10-15", 3, 1)
	before, err := collect(o.reports, o.root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(o.root, sampleFile), "package sample\n\n\n"+sampleSource+"\n")
	issueReport(t, o, sampleFile, "4-9 lines are duplicate of sample.go:11-16", 4, 1)
	after, err := collect(o.reports, o.root)
	if err != nil {
		t.Fatal(err)
	}
	for key, count := range before {
		if after[key] != count || len(after) != len(before) {
			t.Fatal("line movement changed a key")
		}
	}
}

func TestChangedSourceIsNewFinding(t *testing.T) {
	o := fixture(t)
	admit(t, o, 1)
	writeFile(t, filepath.Join(o.root, sampleFile), "package sample\n\nfunc different() {}\n")
	issueReport(t, o, sampleFile, "duplicated with sample.go:3-8", 3, 1)
	expectCheck(t, o, 1)
}

func TestInvalidBaselines(t *testing.T) {
	for name, content := range map[string]string{
		"reason":    `{"version":1,"entries":[{"gate":"golangci","file":"sample.go","rule":"dupl","text":"clone","count":1,"reason":" "}]}`,
		"count":     `{"version":1,"entries":[{"gate":"golangci","file":"sample.go","rule":"dupl","text":"clone","count":0,"reason":"existing"}]}`,
		"line key":  `{"version":1,"entries":[{"gate":"golangci","file":"sample.go","rule":"dupl","text":"clone sample.go:3","count":1,"reason":"existing"}]}`,
		"version":   `{"version":2,"entries":[]}`,
		"duplicate": `{"version":1,"entries":[{"gate":"ruff","file":"sample.py","rule":"F401","text":"unused","count":1,"reason":"existing"},{"gate":"ruff","file":"sample.py","rule":"F401","text":"unused","count":1,"reason":"existing"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			o := fixture(t)
			writeFile(t, o.baseline, content)
			if _, err := readBaseline(o.baseline); err == nil {
				t.Fatal("invalid baseline was accepted")
			}
		})
	}
}

func TestPartialScanOnlyChecksSelectedFiles(t *testing.T) {
	o := fixture(t)
	admit(t, o, 1)
	issueReport(t, o, sampleFile, "new clone", 3, 1)
	o.partial = true
	o.packages = []string{"internal/other"}
	expectCheck(t, o, 0)
	o.packages = []string{"."}
	expectCheck(t, o, 1)
}

func TestThirdPartyIsExcluded(t *testing.T) {
	o := fixture(t)
	issueReport(t, o, "third_party/sample.go", "clone", 3, 1)
	expectCheck(t, o, 0)
}

func TestInvalidReportsFailClosed(t *testing.T) {
	for _, content := range []string{`{}`, `{"Issues":[{"FromLinter":"dupl","Text":"clone","Pos":{"Filename":"../sample.go","Line":3}}]}`, `{"Issues":[{"FromLinter":"dupl","Text":"clone","Pos":{"Filename":"sample.go","Line":99}}]}`} {
		t.Run(content, func(t *testing.T) {
			o := fixture(t)
			writeFile(t, filepath.Join(o.reports, "golangci.json"), content)
			status, err := check(o, io.Discard, io.Discard)
			if err == nil || status != 2 {
				t.Fatalf("invalid report accepted: %d, %v", status, err)
			}
		})
	}
}

func TestOtherToolReports(t *testing.T) {
	o := fixture(t)
	writeFile(t, filepath.Join(o.root, "sample.sh"), "echo $value\n")
	writeFile(t, filepath.Join(o.root, "sample.py"), "import os\n")
	writeFile(t, filepath.Join(o.reports, "deadcode.json"), `[{"Funcs":[{"Name":"abandoned","Position":{"File":"sample.go"}}]}]`)
	writeFile(t, filepath.Join(o.reports, "shellcheck.json"), `[{"file":"sample.sh","code":2086,"message":"quote it","line":1}]`)
	writeFile(t, filepath.Join(o.reports, "ruff.json"), `[{"filename":"sample.py","code":"F401","message":"unused","location":{"row":1}}]`)
	actual, err := collect(o.reports, o.root)
	if err != nil || len(actual) != 3 {
		t.Fatalf("tool reports = %v, %v", actual, err)
	}
	for key := range actual {
		if key.Gate != "deadcode" && strings.TrimSpace(key.Source) == "" {
			t.Fatal("tool finding has no source key")
		}
	}
}
