// Command agent-session-fixture serves finite, explicitly armed passive controls.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

const (
	wrongHost    = "wrong"
	savedSession = "8K3D"
)

type fixture struct {
	mu     sync.Mutex
	events []protocol.Control
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--armed" {
		panic("fixture requires --armed ROOT")
	}
	if err := run(os.Args[2]); err != nil {
		panic(err)
	}
}

func run(root string) error {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open fixture root: %w", err)
	}
	defer directory.Close() //nolint:errcheck // the controller owns the fixture directory
	state := filepath.Join(root, "state")
	local, _, err := identity.LoadOrCreate(state)
	if err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	f := &fixture{}
	hosts, err := f.startHosts(root, local.ID)
	defer hosts.close()
	if err != nil {
		return err
	}

	if err := directory.MkdirAll("config", 0o700); err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	data, err := json.Marshal(map[string]any{"version": 1, "hosts": hosts.records})
	if err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	if err := directory.WriteFile("config/hosts.json", data, 0o600); err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	if err := writeSessions(state); err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	listener, err := net.Listen("unix", filepath.Join(state, "s", "7K3D", "sock"))
	if err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	var connections sync.WaitGroup
	var accept sync.WaitGroup
	accept.Go(func() { acceptConnections(listener, f, local.ID, &connections) })
	defer func() { _ = listener.Close(); accept.Wait(); connections.Wait() }()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"local": local.ID, "a": hosts.records[0].ID, "b": hosts.records[1].ID, wrongHost: hosts.records[2].ID}); err != nil {
		return fmt.Errorf("passive fixture: %w", err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || scanner.Text() != "stop" {
		return fmt.Errorf("fixture requires explicit stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := json.NewEncoder(os.Stdout).Encode(f.events); err != nil {
		return fmt.Errorf("write fixture events: %w", err)
	}
	return nil
}

func acceptConnections(listener net.Listener, f *fixture, host string, group *sync.WaitGroup) {
	for {
		stream, err := listener.Accept()
		if err != nil {
			return
		}
		group.Go(func() {
			defer stream.Close() //nolint:errcheck // the fixture controller joins every connection
			_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
			conn, err := transport.NewStreamConn(stream)
			if err == nil {
				_ = f.serve(conn, host, false)
			}
		})
	}
}

func (f *fixture) serve(conn transport.Conn, host string, wrong bool) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
		response := f.reply(request, host, wrong)
		response.RequestID = request.RequestID
		payload, err := response.Encode()
		if err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
	}
}

func (f *fixture) reply(request protocol.Control, host string, wrong bool) protocol.Control {
	f.mu.Lock()
	f.events = append(f.events, request)
	f.mu.Unlock()
	switch request.Type {
	case protocol.TypeHostInfo:
		if wrong {
			host = "wrong identity"
		}
		return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host, MeshIdentity: host}}
	case protocol.TypeList:
		return protocol.Control{Type: protocol.TypeListed, Sessions: sessionRows(host)}
	case protocol.TypeInspect:
		return inspectReply(request, host)
	default:
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "control.unknown", Message: "unexpected mutation"}
	}
}

func sessionRows(host string) []protocol.SessionInfo {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	exit := 37
	return []protocol.SessionInfo{
		{ID: "7K3D", HostID: host, Command: []string{"sh", "secret argument"}, Cwd: "/private/work", State: "running", CreatedAt: at},
		{ID: savedSession, HostID: host, Command: []string{"sh"}, Cwd: "/private/saved", State: "exited", ExitCode: &exit, CreatedAt: at.Add(-time.Hour), RecoveryPending: true, Hibernated: &recovery.Hibernation{Version: 1, At: at, Reason: "request", Provider: "codex", ConversationID: "fixture"}},
		{ID: "9K3D", HostID: host, Command: []string{"sh"}, Cwd: "/private/unavailable", State: "detached", CreatedAt: at.Add(-2 * time.Hour)},
	}
}

func inspectReply(request protocol.Control, host string) protocol.Control {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if request.SessionID == savedSession {
		record := recovery.Record{Version: 1, HostID: host, SessionID: savedSession, Shell: "/bin/sh", ShellDirectory: "/private/saved", DirectorySource: recovery.DirectoryLaunch, Command: []string{"sh"}, Lines: []string{"saved preview"}}
		if request.PreviewCols == 1 {
			record.Lines = []string{"s"}
		}
		return protocol.Control{Type: protocol.TypeInspected, SessionID: request.SessionID, Recovery: &record}
	}
	if request.SessionID == "9K3D" {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "worker.unavailable", Message: "worker unavailable"}
	}
	if request.SessionID != "7K3D" {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "session.not_found", Message: "session absent"}
	}
	preview := "private preview"
	if request.PreviewCols == 1 {
		preview = "p"
	}
	return protocol.Control{Type: protocol.TypeInspected, SessionID: request.SessionID, Inspection: &protocol.SessionInspection{ObservedAt: at, Attached: true, CurrentDirectory: "/private/current", DirectorySource: protocol.DirectorySourceProcess, ForegroundCommand: "sh", TerminalTitle: "title\x1b[31m", Preview: []string{preview}}}
}

func writeSessions(state string) error {
	root, err := os.OpenRoot(state)
	if err != nil {
		return fmt.Errorf("open fixture state: %w", err)
	}
	defer root.Close() //nolint:errcheck // the controller owns the fixture state
	for _, row := range sessionRows("") {
		dir := filepath.Join("s", row.ID)
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
		meta := worker.Meta{ID: row.ID, PID: os.Getpid(), Command: row.Command, Cwd: row.Cwd, State: row.State, ExitCode: row.ExitCode, CreatedAt: row.CreatedAt, Label: "fixture"}
		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("encode fixture metadata: %w", err)
		}
		if err := root.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600); err != nil {
			return fmt.Errorf("passive fixture: %w", err)
		}
	}
	return nil
}

type fixtureHosts struct {
	records []cli.HostRecord
	servers []*httptest.Server
}

func (h fixtureHosts) close() {
	for _, server := range h.servers {
		server.Close()
	}
}

func (f *fixture) startHosts(root, client string) (fixtureHosts, error) {
	hosts := fixtureHosts{records: make([]cli.HostRecord, 0, 3)}
	for _, name := range []string{"a", "b", wrongHost} {
		host, key, err := identity.LoadOrCreate(filepath.Join(root, name))
		if err != nil {
			return hosts, fmt.Errorf("create fixture host: %w", err)
		}
		auth := &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == client }}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
				return f.serve(conn, host.ID, name == wrongHost)
			})
		}))
		hosts.servers = append(hosts.servers, server)
		hosts.records = append(hosts.records, cli.HostRecord{ID: host.ID, MeshIdentity: host.ID, Endpoint: "ws" + server.URL[4:] + "/control/ws"})
	}
	return hosts, nil
}
