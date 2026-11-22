package mcphost

import (
	"fmt"
	"strings"
	"testing"
)

func askRetry(ref, responses, caps string) string {
	params := strings.Replace(toolParams("ask", `{}`, ""), `{"elicitation":{"form":{}}}`, caps, 1)
	return strings.TrimSuffix(params, "}") + `,"requestState":"` + ref + `","inputResponses":` + responses + `}`
}

const acceptName = `{"name":{"action":"accept","content":{"name":"bob"}}}`

func firstRound(t *testing.T, h *testHost, route Route) string {
	t.Helper()
	out, err := forward(t, h, route, callRequest(t, `1`, toolParams("ask", `{}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := decodeJSONString(member([]byte(out.last()), "result", "requestState"))
	return ref
}

// Every binding of section 6.4 refuses a retry, without consuming the
// reference, and the matching retry still succeeds afterwards.
func TestContinuationChecksEveryBinding(t *testing.T) {
	h := newTestHost(t)
	route := h.route("a")
	ref := firstRound(t, h, route)
	elicit := `{"elicitation":{"form":{}}}`
	cases := map[string]struct {
		route Route
		retry string
	}{
		"grant":              {withRoute(route, func(r *Route) { r.Caller.Grant = "g2" }), askRetry(ref, acceptName, elicit)},
		"revision":           {withRoute(route, func(r *Route) { r.Revision = 2 }), askRetry(ref, acceptName, elicit)},
		"instance":           {withRoute(route, func(r *Route) { r.Instance = "other" }), askRetry(ref, acceptName, elicit)},
		"generation":         {withRoute(route, func(r *Route) { r.Generation = "gen2" }), askRetry(ref, acceptName, elicit)},
		"account":            {withRoute(route, func(r *Route) { r.Account = "someone" }), askRetry(ref, acceptName, elicit)},
		"missing capability": {route, askRetry(ref, acceptName, `{}`)},
		"unrequested input":  {route, askRetry(ref, `{"other":{"action":"accept","content":{}}}`, elicit)},
		"wrong input type":   {route, askRetry(ref, `{"name":{"roots":[]}}`, elicit)},
	}
	for name, c := range cases {
		if _, err := forward(t, h, c.route, callRequest(t, `2`, c.retry)); Classify(err).Code != codeStateInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	out, err := forward(t, h, route, callRequest(t, `3`, askRetry(ref, acceptName, elicit)))
	if err != nil || !strings.Contains(out.last(), "hello bob") {
		t.Fatalf("matching retry after refusals: %s %v", out.last(), err)
	}
}

func withRoute(r Route, change func(*Route)) Route {
	change(&r)
	return r
}

// References live in memory, so a restarted host never honours one.
func TestContinuationIsLostOnRestart(t *testing.T) {
	h := newTestHost(t)
	ref := firstRound(t, h, h.route("a"))
	restarted := newTestHost(t)
	if _, err := forward(t, restarted, restarted.route("a"), callRequest(t, `2`, askRetry(ref, acceptName, `{"elicitation":{"form":{}}}`))); Classify(err).Code != codeStateInvalid {
		t.Fatalf("continuation after restart: %v", err)
	}
}

func TestContinuationBounds(t *testing.T) {
	entry := func(caller string, size int) *continuation {
		return &continuation{binding: continuationBinding{caller: caller}, size: size}
	}
	perCaller := newContinuations()
	for range maxCallerContinuance {
		if _, err := perCaller.store(entry("a", 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := perCaller.store(entry("a", 1)); Classify(err).Code != codeResourceLimit {
		t.Fatalf("per-caller bound: %v", err)
	}
	if _, err := perCaller.store(entry("b", 1)); err != nil {
		t.Fatalf("another caller was limited: %v", err)
	}

	host := newContinuations()
	for i := range maxContinuations {
		if _, err := host.store(entry(fmt.Sprint(i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := host.store(entry("late", 1)); Classify(err).Code != codeResourceLimit {
		t.Fatalf("host count bound: %v", err)
	}

	bytes := newContinuations()
	if _, err := bytes.store(entry("a", continuationBytes-10)); err != nil {
		t.Fatal(err)
	}
	if _, err := bytes.store(entry("b", 11)); Classify(err).Code != codeResourceLimit {
		t.Fatalf("byte bound: %v", err)
	}
}
