package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

func TestInstanceListenCapIsSeparateFromRequests(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	acks := make(chan string, maxInstanceListens)
	for i := range maxInstanceListens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = f.sup.Forward(ctx, caller(fmt.Sprint(i)), Target{Service: "repo"}, listenRequest(t, fmt.Sprintf(`"L%d"`, i)), func(raw []byte, _ bool) error {
				acks <- string(raw)
				return nil
			})
		}(i)
	}
	for range maxInstanceListens {
		waitEvent(t, acks)
	}
	err := f.sup.Forward(t.Context(), caller("extra"), Target{Service: "repo"}, listenRequest(t, `"L-extra"`), (&collected{}).emit)
	if Classify(err).Code != codeResourceLimit {
		t.Fatalf("listen beyond the cap: %v", err)
	}
	if _, err := whoami(t, f.sup, caller("extra"), Target{Service: "repo"}); err != nil {
		t.Fatalf("listens took a request slot: %v", err)
	}
	cancel()
}

// A call in flight when the daemon goes away is never run again.
func TestRestartDoesNotReplayInFlightCalls(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	if reply, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); err != nil || !strings.Contains(reply, "calls=1 ") {
		t.Fatalf("first call: %s %v", reply, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	slow := make(chan error, 1)
	go func() {
		slow <- f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, callRequest(t, `5`, toolParams("slow", `{}`, "")), (&collected{}).emit)
	}()
	eventually(t, func() bool {
		f.sup.mu.Lock()
		defer f.sup.mu.Unlock()
		for _, inst := range f.sup.instances {
			if inst.current != nil && inst.current.requests == 1 {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	f.sup.Close()
	if err := <-slow; Classify(err).Code != codeOutcomeUnknown {
		t.Fatalf("in-flight call across restart: %v", err)
	}
	next := f.open(t)
	reply, err := whoami(t, next, caller("a"), Target{Service: "repo"})
	if err != nil || !strings.Contains(reply, "calls=3 ") {
		t.Fatalf("the slow call was replayed or lost its count: %s %v", reply, err)
	}
}

func TestKeptServiceRestartsWithBackoff(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "kept", InstancePolicy: PolicyShared, Lifecycle: LifecycleKept})
	admin := Caller{Account: "local", Admin: true}
	eventually(t, func() bool {
		v := f.sup.Services(admin)[0]
		return len(v.Instances) == 1 && v.Instances[0].State == StateReady
	})
	f.launcher.stopAll()
	eventually(t, func() bool { return f.sup.Services(admin)[0].Instances[0].State == StateFailed })
	eventually(t, func() bool {
		v := f.sup.Services(admin)[0].Instances[0]
		return v.State == StateReady && v.Generation == 2
	})
}

func TestIncompatibleKeptServiceIsNotRetried(t *testing.T) {
	f := newSupervisorFixture(t)
	f.launcher.mode = "legacy"
	f.publish(t, Definition{Name: "kept", InstancePolicy: PolicyShared, Lifecycle: LifecycleKept})
	admin := Caller{Account: "local", Admin: true}
	eventually(t, func() bool {
		v := f.sup.Services(admin)[0]
		return len(v.Instances) == 1 && v.Instances[0].State == StateIncompatible
	})
	time.Sleep(2 * minBackoff)
	f.launcher.mu.Lock()
	launches := f.launcher.launches
	f.launcher.mu.Unlock()
	if launches != 1 {
		t.Fatalf("an incompatible kept server was relaunched %d times", launches)
	}
}

// A new revision lets the old generation finish its calls before stopping.
func TestRevisionDrainsActiveCalls(t *testing.T) {
	f := newSupervisorFixture(t)
	d := f.publish(t, Definition{Name: "repo", InstancePolicy: PolicyShared})
	if _, err := whoami(t, f.sup, caller("a"), Target{Service: "repo"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	slow := make(chan error, 1)
	go func() {
		slow <- f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, callRequest(t, `9`, toolParams("slow", `{}`, "")), (&collected{}).emit)
	}()
	eventually(t, func() bool {
		f.sup.mu.Lock()
		defer f.sup.mu.Unlock()
		for _, inst := range f.sup.instances {
			if inst.current != nil && inst.current.requests == 1 {
				return true
			}
		}
		return false
	})
	d.Env = map[string]string{"NEW": "1"}
	if _, err := f.sup.Publish(t.Context(), d, d.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := whoami(t, f.sup, caller("b"), Target{Service: "repo"}); err != nil {
		t.Fatal(err)
	}
	if live := f.launcher.live(); live != 2 {
		t.Fatalf("the old generation stopped under an active call: %d live", live)
	}
	cancel()
	if err := <-slow; err == nil {
		t.Fatal("cancelled call succeeded")
	}
	eventually(t, func() bool { return f.launcher.live() == 1 })
}

// headerServer changes a tool's schema after the first listing, so the first
// call carries stale mirrored headers and is refused before anything runs.
func headerServer(t *testing.T, listings, runs *atomic.Int32) string {
	t.Helper()
	return startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		id := requestID(t, body)
		method, _ := decodeJSONString(member(body, "method"))
		switch method {
		case mcpwire.MethodDiscover:
			writeJSON(w, http.StatusOK, `{"jsonrpc":"2.0","id":`+id+`,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":0,"cacheScope":"private"}}`)
		case mcpwire.MethodListTools:
			schema := `{"type":"object"}`
			if listings.Add(1) > 1 {
				schema = regionSchema
			}
			writeJSON(w, http.StatusOK, `{"jsonrpc":"2.0","id":`+id+`,"result":{"resultType":"complete","tools":[{"name":"t","inputSchema":`+schema+`}],"ttlMs":0,"cacheScope":"private"}}`)
		default:
			got, err := mcpwire.DecodeHeaderValue(r.Header.Get("Mcp-Param-Region"))
			if err != nil || got != "eu" {
				writeJSON(w, http.StatusBadRequest, errorFrame(id, mcpwire.HeaderMismatch, "Mcp-Param-Region does not match"))
				return
			}
			runs.Add(1)
			writeJSON(w, http.StatusOK, okResult(id))
		}
	}))
}

func TestHeaderMismatchRefreshesSchemaAndRetriesOnce(t *testing.T) {
	var listings, runs atomic.Int32
	endpoint := headerServer(t, &listings, &runs)
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "web", Argv: []string{}, URL: endpoint, InstancePolicy: PolicyShared})
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	out := &collected{}
	if err := f.sup.Forward(ctx, caller("a"), Target{Service: "web"}, callRequest(t, `"c"`, toolParams("t", `{"region":"eu"}`, "")), out.emit); err != nil {
		t.Fatalf("call after a schema change: %v", err)
	}
	if listings.Load() != 2 || runs.Load() != 1 || len(out.messages) != 1 {
		t.Fatalf("listings %d, runs %d, messages %v", listings.Load(), runs.Load(), out.messages)
	}
}
