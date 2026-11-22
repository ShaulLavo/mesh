package mcphost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testCanonicalHostID() string {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func TestCatalogProjectionExcludesOperationalConfigAndUnknownMeta(t *testing.T) {
	hostID := testCanonicalHostID()
	f := newSupervisorFixture(t)
	f.sup.cfg.HostID = hostID

	const (
		argvSentinel = "SYNTHETIC_ARGV_SECRET_111"
		cwdSentinel  = "SYNTHETIC_CWD_SECRET_222"
		urlSentinel  = "SYNTHETIC_URL_SECRET_333"
		metaSentinel = "SYNTHETIC_META_SECRET_444"
		capSentinel  = "SYNTHETIC_CAP_SECRET_555"
	)

	stdioDef := f.publish(t, Definition{
		Name:           "stdio-svc",
		Transport:      TransportStdio,
		Argv:           []string{"/bin/echo", "--token=" + argvSentinel},
		Cwd:            "/tmp/" + cwdSentinel,
		Env:            map[string]string{"PLAIN_VAR": "plain"},
		InstancePolicy: PolicyShared,
		AccountMode:    AccountHostAccount,
		AccountLabel:   "safe-bot",
	})
	if _, err := f.sup.Publish(t.Context(), Definition{
		Name:           "http-svc",
		Transport:      TransportHTTP,
		URL:            "http://127.0.0.1:9999/mcp/" + urlSentinel,
		InstancePolicy: PolicyShared,
		AccountMode:    AccountNone,
	}, 0); err != nil {
		t.Fatalf("publish http-svc: %v", err)
	}

	// Seed a ready instance with unrecognized upstream serverInfo and capabilities keys.
	inst := &instance{
		rec: InstanceRecord{
			ID:         strings.Repeat("a", 32),
			ServiceID:  stdioDef.ID,
			State:      StateReady,
			Revision:   stdioDef.Revision,
			Generation: 1,
		},
		current: &generation{
			number:       1,
			revision:     stdioDef.Revision,
			serverInfo:   json.RawMessage(`{"name":"safe-srv","version":"2.0","title":"Safe","x-secret":"` + metaSentinel + `"}`),
			capabilities: json.RawMessage(`{"tools":{"listChanged":true},"resources":{"subscribe":true},"completions":{},"x-vendor":"` + capSentinel + `"}`),
		},
	}
	f.sup.mu.Lock()
	f.sup.instances[inst.rec.ID] = inst
	f.sup.mu.Unlock()
	t.Cleanup(func() {
		f.sup.mu.Lock()
		inst.current = nil
		f.sup.mu.Unlock()
	})

	for _, c := range []Caller{{Account: "local", Admin: true}, caller("app-1")} {
		catalog := f.sup.Catalog(c)
		raw, err := json.Marshal(catalog)
		if err != nil {
			t.Fatalf("marshal catalog: %v", err)
		}
		text := string(raw)
		for _, sentinel := range []string{argvSentinel, cwdSentinel, urlSentinel, metaSentinel, capSentinel, `"command"`, `"cwd"`, `"url"`, `"secretEnv"`, `"secretHeaders"`} {
			if strings.Contains(text, sentinel) {
				t.Fatalf("catalog leaked %q for caller %+v: %s", sentinel, c, text)
			}
		}
		if !strings.Contains(text, `"protocolVersion":"2026-07-28"`) || !strings.Contains(text, `"safe-bot"`) || !strings.Contains(text, `"safe-srv"`) {
			t.Fatalf("catalog missing safe metadata: %s", text)
		}
		var got []CatalogService
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal catalog: %v", err)
		}
		if len(got) != 2 || got[1].Name != "stdio-svc" || len(got[1].Instances) != 1 {
			t.Fatalf("unexpected catalog shape: %+v", got)
		}
		caps := got[1].Instances[0].Capabilities
		if caps == nil || caps.Tools == nil || !caps.Tools.ListChanged || caps.Resources == nil || !caps.Resources.Subscribe || !caps.Completions {
			t.Fatalf("typed capabilities not projected: %+v", caps)
		}
	}
}

func TestOwningHostValidatesFullInstanceHandleIncluding32CharServiceName(t *testing.T) {
	hostID := testCanonicalHostID()
	otherHostID := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	f := newSupervisorFixture(t)
	f.sup.cfg.HostID = hostID

	// Publish a service with a 32-character service name (must NOT be confused for a service ID).
	name32 := "service-name-exact-32-characters"
	if len(name32) != 32 {
		t.Fatalf("test setup: len=%d, want 32", len(name32))
	}
	svcA := f.publish(t, Definition{
		Name:           name32,
		Transport:      TransportStdio,
		Argv:           []string{"/bin/echo"},
		InstancePolicy: PolicyIsolated,
		AccountMode:    AccountNone,
	})
	svcB := f.publish(t, Definition{
		Name:           "other-svc",
		Transport:      TransportStdio,
		Argv:           []string{"/bin/echo"},
		InstancePolicy: PolicyIsolated,
		AccountMode:    AccountNone,
	})

	callerA := caller("alice")
	callerB := caller("bob")
	instID := strings.Repeat("c", 32)
	f.sup.mu.Lock()
	f.sup.instances[instID] = &instance{
		rec: InstanceRecord{
			ID:         instID,
			ServiceID:  svcA.ID,
			Owner:      callerA.Key(),
			OwnerGrant: callerA.Grant,
			Default:    true,
			State:      StateStopped,
			Revision:   svcA.Revision,
		},
	}
	f.sup.mu.Unlock()

	validHandle := "mi1." + hostID + "." + svcA.ID + "." + instID
	wrongHostHandle := "mi1." + otherHostID + "." + svcA.ID + "." + instID
	wrongServiceHandle := "mi1." + hostID + "." + svcB.ID + "." + instID
	malformedHandle := "mi1.not-a-host." + svcA.ID + "." + instID

	// 1. Authorized caller with valid handle targeting 32-char service name succeeds.
	got, _, err := f.sup.pick(t.Context(), callerA, Target{Service: name32, Instance: validHandle})
	if err != nil || got == nil || got.rec.ID != instID {
		t.Fatalf("authorized handle pick failed: got=%v err=%v", got, err)
	}

	// 2. Wrong host in handle is rejected.
	if _, _, err := f.sup.pick(t.Context(), callerA, Target{Service: name32, Instance: wrongHostHandle}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong host handle = %v, want ErrInvalid", err)
	}

	// 3. Wrong service ID in handle is rejected even when instance ID exists for svcA.
	if _, _, err := f.sup.pick(t.Context(), callerA, Target{Service: name32, Instance: wrongServiceHandle}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong service handle = %v, want ErrNotFound", err)
	}

	// 4. Malformed handle is rejected.
	if _, _, err := f.sup.pick(t.Context(), callerA, Target{Service: name32, Instance: malformedHandle}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed handle = %v, want ErrInvalid", err)
	}

	// 5. Another caller using caller A's valid handle is rejected with ErrDenied.
	if _, _, err := f.sup.pick(t.Context(), callerB, Target{Service: name32, Instance: validHandle}); !errors.Is(err, ErrDenied) {
		t.Fatalf("other caller handle = %v, want ErrDenied", err)
	}
}
