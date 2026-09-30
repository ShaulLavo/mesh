package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func nonGoReport(t *testing.T, o options, gate, file string, lines []int) {
	t.Helper()
	issues := make([]any, 0, len(lines))
	for _, line := range lines {
		if gate == shellcheckGate {
			issues = append(issues, map[string]any{"file": file, "code": 2086, "message": "Double quote to prevent globbing and word splitting.", "line": line})
		} else {
			issues = append(issues, map[string]any{"filename": file, "code": "E402", "message": "Module level import not at top of file", "location": map[string]int{"row": line}})
		}
	}
	data, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(o.reports, gate+".json"), string(data))
}

func TestNonGoReplacementIsNewAndUpdateRefuses(t *testing.T) {
	for _, tc := range []struct {
		gate, file, before, after string
		beforeLines, afterLines   []int
	}{
		{shellcheckGate, "linux.sh", "#!/bin/sh\nexport XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$remote_uid}\nexport OTHER=$value\ninstall -m 0755 \"$source_binary\" \"$binary_tmp\"\n", "#!/bin/sh\nexport XDG_RUNTIME_DIR=\"${XDG_RUNTIME_DIR:-/run/user/$remote_uid}\"\nexport OTHER=$value\ninstall -m 0755 $source_binary \"$binary_tmp\"\n", []int{2, 3}, []int{3, 4}},
		{ruffGate, "sample.py", "value = 1\nimport os\nimport sys\n", "import os\nvalue = 1\nimport sys\nimport pathlib\n", []int{2, 3}, []int{3, 4}},
	} {
		t.Run(tc.gate, func(t *testing.T) {
			o := fixture(t)
			writeFile(t, filepath.Join(o.root, tc.file), tc.before)
			nonGoReport(t, o, tc.gate, tc.file, tc.beforeLines)
			actual, err := collect(o.reports, o.root)
			if err != nil {
				t.Fatal(err)
			}
			entries := make(map[findingKey]entry)
			for key, count := range actual {
				entries[key] = entry{findingKey: key, Count: count, Reason: "Existing statement deferred; unrelated replacements are not admitted."}
			}
			if err := writeBaseline(o.baseline, entries); err != nil {
				t.Fatal(err)
			}
			expectCheck(t, o, 0)
			writeFile(t, filepath.Join(o.root, tc.file), tc.after)
			nonGoReport(t, o, tc.gate, tc.file, tc.afterLines)
			expectCheck(t, o, 1)
			before := bytesAt(t, o.baseline)
			o.update = true
			expectCheck(t, o, 1)
			if bytesAt(t, o.baseline) != before {
				t.Fatal("replacement was admitted by the removal-only updater")
			}
		})
	}
}
