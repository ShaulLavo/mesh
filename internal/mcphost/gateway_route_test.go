package mcphost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// A gateway routes by stable ID. A retired ID that is also the name of a
// different service never reaches that service.
func TestExactIDNeverMatchesAName(t *testing.T) {
	f := newSupervisorFixture(t)
	old := f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	if err := f.sup.Remove(t.Context(), "repo"); err != nil {
		t.Fatal(err)
	}
	f.publish(t, Definition{Name: old.ID, InstancePolicy: PolicyShared})
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: old.ID}); err != nil {
		t.Fatalf("name lookup still works for ordinary callers: %v", err)
	}
	launches := f.launcher.launchCount()
	_, err := whoami(t, f.sup, caller("a"), Target{Service: old.ID, ExactID: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a retired ID reached the service named after it: %v", err)
	}
	if f.launcher.launchCount() != launches {
		t.Fatal("an exact miss started a worker")
	}
}

// A request resolved against an older revision or account is refused before
// any instance is chosen.
func TestExpectationRefusesAMovedService(t *testing.T) {
	f := newSupervisorFixture(t)
	d := f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	target := Target{Service: d.ID, ExactID: true, Expect: &Expectation{Revision: d.Revision, Account: d.AccountLabel}}
	if _, err := whoami(t, f.sup, caller("a"), target); err != nil {
		t.Fatal(err)
	}
	d.Env = map[string]string{"REV": "2"}
	next, err := f.sup.Publish(t.Context(), d, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var fail *Failure
	if _, err := whoami(t, f.sup, caller("a"), target); !errors.As(err, &fail) || fail.Code != codeCatalogStale || !fail.Retryable {
		t.Fatalf("a call resolved at revision %d reached revision %d: %v", d.Revision, next.Revision, err)
	}
	wrongAccount := Target{Service: d.ID, ExactID: true, Expect: &Expectation{Revision: next.Revision, Account: "someone"}}
	if _, err := whoami(t, f.sup, caller("a"), wrongAccount); !errors.As(err, &fail) || fail.Code != codeCatalogStale {
		t.Fatalf("a call resolved for another account went through: %v", err)
	}
}

// An obsolete continuation is refused as invalid state without choosing,
// rebinding or starting an instance.
func TestObsoleteContinuationNeedsNoWorker(t *testing.T) {
	f := newSupervisorFixture(t)
	d := f.publish(t, Definition{Name: "repo"})
	a := caller("a")
	out := &collected{}
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	if err := f.sup.Forward(ctx, a, Target{Service: "repo"}, callRequest(t, `1`, toolParams("ask", `{}`, "")), out.emit); err != nil {
		t.Fatal(err)
	}
	ref, _ := decodeJSONString(member([]byte(out.last()), "result", "requestState"))
	d.Env = map[string]string{"REV": "2"}
	if _, err := f.sup.Publish(t.Context(), d, d.Revision); err != nil {
		t.Fatal(err)
	}
	launches := f.launcher.launchCount()
	err := f.sup.Forward(ctx, a, Target{Service: "repo"}, callRequest(t, `2`, askRetry(ref, acceptName, `{"elicitation":{}}`)), (&collected{}).emit)
	var fail *Failure
	if !errors.As(err, &fail) || fail.Code != codeStateInvalid {
		t.Fatalf("obsolete continuation: %v", err)
	}
	if f.launcher.launchCount() != launches {
		t.Fatal("rejecting an obsolete continuation started a worker")
	}
	other := caller("b")
	err = f.sup.Forward(ctx, other, Target{Service: "repo"}, callRequest(t, `3`, askRetry("mesh-c1.unknown", acceptName, `{"elicitation":{}}`)), (&collected{}).emit)
	if !errors.As(err, &fail) || fail.Code != codeStateInvalid || f.launcher.launchCount() != launches {
		t.Fatalf("an unknown reference from a new caller: %v, launches %d", err, f.launcher.launchCount()-launches)
	}
}

// The host advances a service's listing revision when a listing passing
// through changes or an upstream reports a list change, and reports it.
func TestListingRevisionAdvancesOnChange(t *testing.T) {
	var mu sync.Mutex
	var seen []ListingRevision
	f := newSupervisorFixture(t)
	f.sup.router.listings.notify = func(rev ListingRevision) {
		mu.Lock()
		seen = append(seen, rev)
		mu.Unlock()
	}
	d := f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	token := func() string {
		for _, s := range f.sup.Catalog(caller("a")) {
			if s.ID == d.ID {
				return s.ListingRevision
			}
		}
		return ""
	}
	start := token()
	if !strings.HasPrefix(start, "1.") {
		t.Fatalf("token %q does not name the definition revision", start)
	}
	route := Route{Service: d.ID, Revision: d.Revision}
	list := func(items string) {
		req := parseRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
		reply := parseMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":`+items+`,"ttlMs":0,"cacheScope":"private"}}`)
		f.sup.router.listings.observeEvent(route, req, reply)
	}
	list(`[{"name":"a","inputSchema":{"type":"object"}}]`)
	list(`[{"name":"a","inputSchema":{"type":"object"}}]`)
	if token() != start {
		t.Fatal("an unchanged listing advanced the revision")
	}
	list(`[{"name":"b","inputSchema":{"type":"object"}}]`)
	changed := token()
	if changed == start {
		t.Fatal("a changed listing kept its revision")
	}
	note := parseMessage(t, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}`)
	f.sup.router.listings.observeEvent(route, note, note)
	if token() == changed {
		t.Fatal("an upstream list change kept the revision")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 3 || seen[len(seen)-1].Token != token() {
		t.Fatalf("notifications %+v", seen)
	}
}

func (f *fakeLauncher) launchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches
}

func parseMessage(t *testing.T, raw string) mcpwire.Message {
	t.Helper()
	m, fault := mcpwire.Parse([]byte(raw))
	if fault != nil {
		t.Fatalf("parse %s: %v", raw, fault)
	}
	return m
}

// Review 13: a lease is keyed by the caller, so repeating a creation after a
// lost reply returns the same lease instead of adding another.
func TestRepeatedLeaseKeyReturnsTheSameLease(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	a := caller("a")
	first, _, err := f.sup.Lease(t.Context(), a, Target{Service: "repo"}, "job-key", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	again, view, err := f.sup.Lease(t.Context(), a, Target{Service: "repo"}, "job-key", time.Hour, "")
	if err != nil || again.ID != first.ID || view.Leases != 1 {
		t.Fatalf("a repeated key made another lease: %+v %+v %v", again, view, err)
	}
	if _, _, err := f.sup.Lease(t.Context(), caller("b"), Target{Service: "repo"}, "job-key", time.Hour, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("another caller reused the key: %v", err)
	}
}
