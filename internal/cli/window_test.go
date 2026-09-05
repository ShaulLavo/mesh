package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestWindowFlagsRejectInvalidEntryBeforeDiscovery(t *testing.T) {
	setupCommandTestHost(t)
	for _, args := range [][]string{
		{"--window"}, {"--take"}, {"--window", "pc"}, {"--window", "--resume"}, {"--window", "--", "bash"},
	} {
		_, _, err := executeCommand(t, Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			t.Fatal("invalid window command queried the network")
			return nil, errors.New("unexpected dial")
		}}, args...)
		if err == nil {
			t.Fatalf("%v succeeded", args)
		}
	}
	rows, err := List()
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid window command created sessions: %#v, %v", rows, err)
	}
}

func TestPickerStartsWithLocalCatalogBeforeRemoteDiscovery(t *testing.T) {
	host := setupCommandTestHost(t)
	called := false
	_, _, err := executeCommand(t, Dependencies{
		DialHost: host.dial,
		Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
			called = true
			if len(host.recorded()) != 0 || len(input.Hosts) != 2 || !input.Hosts[0].Local || input.Hosts[0].Host.Alias != localHostAlias {
				t.Fatalf("picker did not receive local catalog before network: %#v, events %v", input.Hosts, host.recorded())
			}
			local, err := input.Refresh(ctx, localHostAlias)
			if err != nil || !local.Sessions.Local || len(host.recorded()) != 0 {
				t.Fatalf("local refresh used remote discovery: %#v, %v", local, err)
			}
			loaded, err := input.LoadHosts(ctx)
			if err != nil || len(loaded) != 1 || loaded[0].Host.Alias != "pc" || loaded[0].Stale {
				t.Fatalf("async catalog = %#v, %v", loaded, err)
			}
			return PickerSelection{}, nil
		},
	})
	if err != nil || !called {
		t.Fatalf("picker result = %v, called %t", err, called)
	}
}

func TestPickerResumeNeverTakesAnAttachedSession(t *testing.T) {
	for _, state := range []string{worker.StateRunning, worker.StateDetached} {
		t.Run(state, func(t *testing.T) {
			host := setupCommandTestHost(t)
			host.sessionState = state
			_, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Picker: func(context.Context, PickerInput) (PickerSelection, error) {
				return PickerSelection{HostAlias: "pc"}, nil
			}}, "--raw")
			if state == worker.StateRunning {
				if err == nil || !strings.Contains(err.Error(), "no detached sessions") {
					t.Fatalf("resume in-use session = %v", err)
				}
				if host.eventCount(protocol.TypeAttach)+host.eventCount(protocol.TypeAttachDetached) != 0 {
					t.Fatal("resume attached a session already in use")
				}
			} else if err != nil || host.attached().Type != protocol.TypeAttachDetached {
				t.Fatalf("detached resume = %#v, %v", host.attached(), err)
			}
		})
	}
}

func TestPickerRelaunchUsesRecordedCommandAndDirectory(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed creation keeps old record"}[fail], func(t *testing.T) {
			host := setupCommandTestHost(t)
			host.sessionState, host.createdID = worker.StateInterrupted, "91AZ"
			if fail {
				host.createError = "launch directory is gone"
			}
			_, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial,
				Picker: func(context.Context, PickerInput) (PickerSelection, error) {
					return PickerSelection{HostAlias: "pc", SessionID: "7K3D", Relaunch: true}, nil
				},
			}, "--raw")
			if fail {
				if err == nil || host.eventCount(protocol.TypeRemove) != 0 || host.attached().Type != "" {
					t.Fatalf("failed creation removed or attached: %v, events %v", err, host.recorded())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if host.create.Cwd != "/work" || !slices.Equal(host.create.Command, []string{"bash"}) {
				t.Fatalf("relaunch create = %#v", host.create)
			}
			if host.actedOn().SessionID != "7K3D" || host.attached().SessionID != "91AZ" || host.attached().LastSeq == nil || *host.attached().LastSeq != 0 {
				t.Fatalf("relaunch removed %#v and attached %#v", host.actedOn(), host.attached())
			}
		})
	}
}

func TestOfflineForgetLeavesDurableMarkerAndHidesLocalRecord(t *testing.T) {
	setupCommandTestHost(t)
	dir, err := paths.SessionDir("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := worker.Meta{ID: "7K3D", State: worker.StateRunning, Command: []string{"bash"}, Cwd: "/work", CreatedAt: commandTestTime}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	current := Session{Meta: meta, Dir: dir}
	for range 2 {
		if err := forgetLocalSession(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(paths.Forgotten(dir)); err != nil {
		t.Fatal(err)
	}
	rows, err := List()
	if err != nil || len(rows) != 0 {
		t.Fatalf("forgotten session remains visible: %#v, %v", rows, err)
	}
}

func TestLeaveKeyCannotShadowDefaultInnerDetach(t *testing.T) {
	command := NewCommand(Dependencies{})
	if err := command.PersistentFlags().Set("leave-key", "ctrl+]"); err != nil {
		t.Fatal(err)
	}
	app := &application{}
	if _, err := app.attachmentOptions(command, "", false); err == nil {
		t.Fatal("leave-all was allowed to shadow the default detach key")
	}
	if _, err := app.attachmentOptions(command, "", true); err != nil {
		t.Fatalf("raw input should ignore key collisions: %v", err)
	}
}
