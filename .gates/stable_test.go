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
