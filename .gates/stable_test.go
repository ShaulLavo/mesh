package main

import "testing"

func TestVolatileMessagesHaveStableIdentity(t *testing.T) {
	for _, tc := range []struct{ rule, first, second string }{
		{"goconst", "string `127.0.0.1` has 18 occurrences, make it a constant", "string `127.0.0.1` has 19 occurrences, make it a constant"},
		{"goconst", "string `/mesh` has 26 occurrences, make it a constant", "string `/mesh` has 27 occurrences, but such constant `defaultWebSocketPath` already exists"},
		{"gocognit", "cognitive complexity 20 of func `sample` is high (> 15)", "cognitive complexity 21 of func `sample` is high (> 15)"},
		{"nilerr", "error is not nil (line 197) but it returns nil", "error is not nil (line 198) but it returns nil"},
		{"nestif", "`if n > 1` has complex nested blocks (complexity: 4)", "`if n > 1` has complex nested blocks (complexity: 5)"},
	} {
		t.Run(tc.rule+tc.first, func(t *testing.T) {
			o := fixture(t)
			ruleIssueReport(t, o, tc.rule, sampleFile, tc.first, 3, 1)
			first, err := collect(o.reports, o.root)
			if err != nil {
				t.Fatal(err)
			}
			entries := make(map[findingKey]entry)
			for key, count := range first {
				entries[key] = entry{findingKey: key, Count: count, Reason: "Existing finding; diagnostics must not churn its identity."}
			}
			if err := writeBaseline(o.baseline, entries); err != nil {
				t.Fatal(err)
			}
			ruleIssueReport(t, o, tc.rule, sampleFile, tc.second, 3, 1)
			expectCheck(t, o, 0)
		})
	}
}

func TestAlignmentAndRepresentativeOccurrenceDoNotChangeIdentity(t *testing.T) {
	for _, rule := range []string{"wrapcheck", "goconst"} {
		t.Run(rule, func(t *testing.T) {
			o := fixture(t)
			writeFile(t, o.root+"/"+sampleFile, "package sample\nfunc sample() {\n _ = struct{ A, Longer string }{\n A: \"boot-a\",\n Longer: \"other\",\n }\n}\n")
			text := "external error is unwrapped"
			if rule == "goconst" {
				text = "string `boot-a` has 37 occurrences, make it a constant"
			}
			ruleIssueReport(t, o, rule, sampleFile, text, 4, 1)
			actual, err := collect(o.reports, o.root)
			if err != nil {
				t.Fatal(err)
			}
			entries := make(map[findingKey]entry)
			for key, count := range actual {
				entries[key] = entry{findingKey: key, Count: count, Reason: "Existing finding; formatting does not fix it."}
			}
			if err := writeBaseline(o.baseline, entries); err != nil {
				t.Fatal(err)
			}
			writeFile(t, o.root+"/"+sampleFile, "package sample\n\nfunc sample() {\n _ = struct{ A, Longer string }{\n A:      \"boot-a\",\n Longer: \"other\",\n }\n}\n")
			ruleIssueReport(t, o, rule, sampleFile, text, 5, 1)
			expectCheck(t, o, 0)
			ruleIssueReport(t, o, rule, sampleFile, text, 5, 2)
			expectCheck(t, o, 1)
		})
	}
}

func TestLiteralDigitsRemainPartOfIdentity(t *testing.T) {
	for _, tc := range []struct{ rule, first, second string }{
		{"goconst", "string `127.0.0.1` has 3 occurrences, make it a constant", "string `127.0.0.2` has 3 occurrences, make it a constant"},
		{"goconst", "string `file.go:1` has 3 occurrences, make it a constant", "string `file.go:2` has 3 occurrences, make it a constant"},
		{"nestif", "`if s == \"complexity: 4\"` has complex nested blocks (complexity: 5)", "`if s == \"complexity: 6\"` has complex nested blocks (complexity: 5)"},
		{"nestif", "`if s == \"file.go:1\"` has complex nested blocks (complexity: 5)", "`if s == \"file.go:2\"` has complex nested blocks (complexity: 5)"},
	} {
		t.Run(tc.rule+tc.first, func(t *testing.T) {
			if normalizeText(tc.rule, tc.first) == normalizeText(tc.rule, tc.second) {
				t.Fatal("different literals merged")
			}
		})
	}
}
