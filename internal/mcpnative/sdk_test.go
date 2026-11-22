package mcpnative

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

func TestSDKClientPositivePeerInterop(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	controls := &stubControls{
		outputFn: func(_ context.Context, req JobOutputRequest) (CallOutcome, error) {
			if req.Cursor == "expired" {
				return CallOutcome{
					Envelope: agentapi.Envelope{
						Operation: "sessions.logs",
						Status:    agentapi.StatusError,
						Data:      JobOutputData{Job: req.Job.String(), Stream: req.Stream, Gap: true, EarliestCursor: "earliest-tok"},
						Errors:    []agentapi.Error{agentapi.Fail(agentapi.LogsCursorExpired, "sessions.logs", req.Job.HostID, "cursor expired").Err},
					},
					IsError: true,
				}, nil
			}
			target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc", Account: "agent"}
			return CallOutcome{
				Envelope: okEnvelope("sessions.logs", JobOutputData{Job: req.Job.String(), Target: *target, Stream: req.Stream, Encoding: "utf-8", Data: "sdk-hello\n", EOF: true}),
				Target:   target,
			}, nil
		},
	}

	server, err := NewStdioServer(ServerOptions{Version: "0.1.0-sdk", Controls: controls})
	if err != nil {
		t.Fatal(err)
	}
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(t.Context(), serverIn, serverOut)
		_ = serverOut.Close()
	}()
	t.Cleanup(func() {
		_ = clientOut.Close()
		_ = clientIn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stdio server did not exit")
		}
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "go-sdk-peer", Version: "1.8.0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, &mcp.ClientSessionOptions{
		ProtocolVersion: mcpwire.Version,
	})
	if err != nil {
		t.Fatalf("SDK client connect failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("SDK ListTools: %v", err)
	}
	if len(listed.Tools) != len(DefaultRegistry().Definitions()) {
		t.Fatalf("SDK ListTools count = %d, want %d", len(listed.Tools), len(DefaultRegistry().Definitions()))
	}

	skillRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_skill"})
	if err != nil || skillRes.IsError || len(skillRes.Content) == 0 {
		t.Fatalf("SDK mesh_skill: res=%+v err=%v", skillRes, err)
	}

	hostsRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_hosts_list"})
	if err != nil || hostsRes.IsError || hostsRes.StructuredContent == nil {
		t.Fatalf("SDK mesh_hosts_list: res=%+v err=%v", hostsRes, err)
	}
	rawEnv, err := json.Marshal(hostsRes.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var env agentapi.Envelope
	if err := json.Unmarshal(rawEnv, &env); err != nil || env.Status != agentapi.StatusOK || env.Operation != "hosts.list" {
		t.Fatalf("SDK structuredContent envelope = %s, err=%v", rawEnv, err)
	}

	outRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mesh_job_output",
		Arguments: map[string]any{"job": testJob, "stream": "stdout"},
	})
	if err != nil || outRes.IsError {
		t.Fatalf("SDK mesh_job_output: res=%+v err=%v", outRes, err)
	}

	// Context open and mesh_exec round-trip through Go SDK v1.8.0.
	ctxRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mesh_context_open",
		Arguments: map[string]any{"host": testHostID, "request_key": "sdk-ctx", "cwd": "/work"},
	})
	if err != nil || ctxRes.IsError || ctxRes.StructuredContent == nil {
		t.Fatalf("SDK mesh_context_open: res=%+v err=%v", ctxRes, err)
	}

	execRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mesh_exec",
		Arguments: map[string]any{"context": testContext, "request_key": "sdk-exec", "argv": []string{"/bin/true"}},
	})
	if err != nil || execRes.IsError || execRes.StructuredContent == nil {
		t.Fatalf("SDK mesh_exec: res=%+v err=%v", execRes, err)
	}

	// Tool error carries isError:true and the agentapi envelope in structuredContent.
	errRes, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mesh_job_output",
		Arguments: map[string]any{"job": testJob, "stream": "stdout", "cursor": "expired"},
	})
	if err != nil || !errRes.IsError || errRes.StructuredContent == nil {
		t.Fatalf("SDK tool error: res=%+v err=%v", errRes, err)
	}
}
