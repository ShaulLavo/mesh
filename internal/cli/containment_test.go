package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func TestQueryContainingSessionWorkerReturnsExactNestedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "7K3D")
	if err := mkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // test cleanup
	want := []protocol.SessionIdentity{
		{HostID: "host-b", SessionID: "7K3D"},
		{HostID: "host-a", SessionID: "A111"},
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close() //nolint:errcheck // test cleanup
		frame, err := protocol.NewReader(conn).ReadFrame()
		if err != nil {
			serverErr <- err
			return
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			serverErr <- err
			return
		}
		if request.Type != protocol.TypeContainment || request.SessionID != "7K3D" {
			serverErr <- errors.New("unexpected containment request")
			return
		}
		serverErr <- protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
			Type:               protocol.TypeContained,
			RequestID:          request.RequestID,
			SessionID:          request.SessionID,
			ContainingSessions: want,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := queryContainingSessionWorker(ctx, worker.SessionWorkerLocation{SessionID: "7K3D", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("containing sessions = %#v, want %#v", got, want)
	}
	got[0].HostID = "mutated"
	if want[0].HostID != "host-b" {
		t.Fatal("query result aliases response storage")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLegacyContainingSessionUsesOnlyProvenLocalIdentity(t *testing.T) {
	location := worker.SessionWorkerLocation{SessionID: "7K3D", Dir: "/state/s/7K3D"}
	loadedFrom := ""
	load := func(stateDir string) (identity.Host, error) {
		loadedFrom = stateDir
		return identity.Host{ID: "local-host"}, nil
	}

	got := legacyContainingSession(location, "7k3d", "environment-host", load)
	want := []protocol.SessionIdentity{{HostID: "environment-host", SessionID: "7K3D"}}
	if !slices.Equal(got, want) || loadedFrom != "" {
		t.Fatalf("environment identity = %#v, load path %q", got, loadedFrom)
	}

	got = legacyContainingSession(location, "different", "remote-host", load)
	want = []protocol.SessionIdentity{{HostID: "local-host", SessionID: "7K3D"}}
	if !slices.Equal(got, want) || loadedFrom != "/state" {
		t.Fatalf("local identity = %#v, load path %q", got, loadedFrom)
	}

	missing := legacyContainingSession(location, "", "remote-host", func(string) (identity.Host, error) {
		return identity.Host{}, errors.New("no local identity")
	})
	if len(missing) != 0 {
		t.Fatalf("unproven legacy identity = %#v, want none", missing)
	}
}

func TestResolvedTargetIdentityLoadsTheOwnerOfAValidatedLocalSessionDirectory(t *testing.T) {
	stateDir := t.TempDir()
	local, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(stateDir, "s", "7K3D")
	if err := mkdirPrivate(sessionDir); err != nil {
		t.Fatal(err)
	}
	target, err := resolvedTargetIdentity(resolvedSession{local: &Session{
		Meta: worker.Meta{ID: "7K3D"},
		Dir:  sessionDir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.SessionIdentity{HostID: local.ID, SessionID: "7K3D"}
	if target != want {
		t.Fatalf("local target identity = %#v, want %#v", target, want)
	}

	if _, err := resolvedTargetIdentity(resolvedSession{local: &Session{
		Meta: worker.Meta{ID: "7K3D"},
		Dir:  filepath.Join(stateDir, "other", "7K3D"),
	}}); err == nil {
		t.Fatal("local target outside a validated sessions directory was accepted")
	}
}

func mkdirPrivate(path string) error {
	return os.MkdirAll(path, 0o700)
}

func TestValidateAttachOptionsRejectsContainingTarget(t *testing.T) {
	stateDir := t.TempDir()
	local, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		opts AttachOptions
	}{
		{name: "known host", opts: AttachOptions{HostID: local.ID}},
		{name: "direct worker socket", opts: AttachOptions{SocketPath: paths.Socket(filepath.Join(stateDir, "s", "AAAA"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := test.opts
			opts.SessionID = "AAAA"
			opts.ContainingSessions = []protocol.SessionIdentity{{HostID: local.ID, SessionID: "AAAA"}}
			if _, err := validateAttachOptions(opts); err == nil || !strings.Contains(err.Error(), "already contains this terminal") {
				t.Fatalf("self attachment validation = %v", err)
			}
			opts.ContainingSessions[0].HostID = "different-host"
			if _, err := validateAttachOptions(opts); err != nil {
				t.Fatalf("same session ID on another host was rejected: %v", err)
			}
		})
	}
}
