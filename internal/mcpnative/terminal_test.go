package mcpnative

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
)

func (s *stubControls) TerminalOpen(_ context.Context, req TerminalOpenRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.create", JobStateData{Job: testJob, RequestKey: req.RequestKey, State: "running"})}, nil
}
func (s *stubControls) TerminalResize(_ context.Context, req TerminalResizeRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.resize", TerminalSizeData{Job: req.Job.String(), Size: agentapi.TerminalSize{Cols: req.Cols, Rows: req.Rows}})}, nil
}
func (s *stubControls) TerminalScreen(_ context.Context, req TerminalScreenRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.screen", TerminalScreenData{Job: req.Job.String(), Screen: agentapi.TerminalScreen{Lines: []string{"$"}, Cols: req.Cols, Rows: req.Rows}})}, nil
}
func (s *stubControls) StartService(context.Context, ServiceRouteRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.start", agentapi.ServiceChange{Warnings: []string{}})}, nil
}
func (s *stubControls) StopService(context.Context, ServiceRouteRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.stop", agentapi.ServiceChange{Warnings: []string{}})}, nil
}
func (s *stubControls) PublishService(context.Context, PublishServiceRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.publish", agentapi.ServiceChange{Warnings: []string{}})}, nil
}
func (s *stubControls) UnpublishService(_ context.Context, req ServiceRouteRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.unpublish", agentapi.ServiceRemoval{Name: req.Name, Removed: true})}, nil
}
func (s *stubControls) ListApps(context.Context, ListAppsRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("apps.list", agentapi.AppsData{Apps: []agentapi.AppRow{}})}, nil
}
func (s *stubControls) HibernateSession(_ context.Context, req SessionLifecycleRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.hibernate", agentapi.SessionHibernation{SessionID: req.SessionID, Hibernated: true})}, nil
}
func (s *stubControls) RecoverSession(_ context.Context, req SessionLifecycleRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.recover", agentapi.SessionRecovery{RecoveredFrom: req.SessionID, Action: req.Action})}, nil
}

// inputRecorder keeps the kind each input tool asked for.
type inputRecorder struct {
	stubControls
	inputs []JobInputRequest
}

func (r *inputRecorder) JobInput(ctx context.Context, req JobInputRequest) (CallOutcome, error) {
	r.inputs = append(r.inputs, req)
	return r.stubControls.JobInput(ctx, req)
}

func callTool(t *testing.T, controls Controls, name, args string) (CallOutcome, error) {
	t.Helper()
	spec, ok := DefaultRegistry().Lookup(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	return spec.Handler(t.Context(), controls, json.RawMessage(args), nil)
}

func isParamError(err error) bool {
	var param *paramError
	return errors.As(err, &param)
}

// Terminal keys and pipe stdin never mix: each tool names the job kind it
// writes to, and the host refuses the other kind.
func TestTerminalAndPipeInputNameTheirKind(t *testing.T) {
	recorder := &inputRecorder{}
	if _, err := callTool(t, recorder, "mesh_terminal_input", `{"job":"`+testJob+`","write_key":"k1","offset":"0","data":"ls\r"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, recorder, "mesh_job_input", `{"job":"`+testJob+`","write_key":"k2","offset":"0","eof":true}`); err != nil {
		t.Fatal(err)
	}
	if len(recorder.inputs) != 2 || recorder.inputs[0].Kind != jobKindTerminal || recorder.inputs[1].Kind != jobKindPipe {
		t.Fatalf("input kinds = %+v", recorder.inputs)
	}
	if string(recorder.inputs[0].Data) != "ls\r" || recorder.inputs[0].EOF {
		t.Fatalf("terminal write = %+v", recorder.inputs[0])
	}
	// A terminal has no end of file; the tool does not accept one.
	if _, err := callTool(t, recorder, "mesh_terminal_input", `{"job":"`+testJob+`","write_key":"k3","offset":"3","eof":true}`); !isParamError(err) {
		t.Fatalf("terminal eof = %v, want a parameter error", err)
	}
}

func TestTerminalToolArgumentLimits(t *testing.T) {
	controls := &stubControls{}
	cases := []struct {
		tool, args string
		ok         bool
	}{
		{"mesh_terminal_open", `{"context":"` + testContext + `","request_key":"t1","argv":["/bin/sh"]}`, true},
		{"mesh_terminal_open", `{"context":"` + testContext + `","request_key":"t1","argv":["/bin/sh"],"cols":4096}`, false},
		{"mesh_terminal_open", `{"context":"` + testContext + `","request_key":"t1"}`, false},
		{"mesh_terminal_open", `{"context":"` + testContext + `","request_key":"t1","argv":["/bin/sh"],"stdin":"open"}`, false},
		{"mesh_terminal_resize", `{"job":"` + testJob + `","cols":120,"rows":40}`, true},
		{"mesh_terminal_resize", `{"job":"` + testJob + `","cols":120}`, false},
		{"mesh_terminal_screen", `{"job":"` + testJob + `"}`, true},
		{"mesh_terminal_screen", `{"job":"` + testJob + `","cols":161}`, false},
		{"mesh_terminal_screen", `{"job":"` + testJob + `","context":"mc1.otherhost.` + testContextID + `"}`, false},
		{"mesh_service_start", `{"host":"pc","name":"/dev"}`, true},
		{"mesh_service_stop", `{"host":"pc"}`, false},
		{"mesh_service_publish", `{"host":"pc","target":"5173","run":"bun run dev","cwd":"/work/app","keep_running":true}`, true},
		{"mesh_service_publish", `{"host":"pc"}`, false},
		{"mesh_apps_list", `{"host":"local"}`, true},
		{"mesh_session_hibernate", `{"host":"pc","session_id":"7K3D"}`, true},
		{"mesh_session_hibernate", `{"host":"pc","session_id":"7K3D","action":"shell"}`, false},
		{"mesh_session_recover", `{"host":"pc","session_id":"7K3D","action":"shell"}`, true},
		{"mesh_session_recover", `{"host":"pc","session_id":"7K3D","action":"attach"}`, false},
	}
	for _, tc := range cases {
		out, err := callTool(t, controls, tc.tool, tc.args)
		if tc.ok && (err != nil || out.Envelope.Status != agentapi.StatusOK) {
			t.Errorf("%s %s = %+v, %v", tc.tool, tc.args, out.Envelope, err)
		}
		if !tc.ok && !isParamError(err) {
			t.Errorf("%s %s = %v, want a parameter error", tc.tool, tc.args, err)
		}
	}
}

// Every new tool links an operation that agentapi knows, so CLI and MCP
// results share one envelope vocabulary.
func TestTerminalAndServiceToolsLinkKnownOperations(t *testing.T) {
	known := map[string]bool{}
	for _, operation := range agentapi.Operations {
		known[operation.Name] = true
	}
	for _, spec := range append(terminalToolSpecs(), serviceToolSpecs()...) {
		if !known[spec.Operation] {
			t.Errorf("%s links unknown operation %q", spec.Name, spec.Operation)
		}
		if !json.Valid(spec.InputSchema) {
			t.Errorf("%s has an invalid input schema", spec.Name)
		}
		readOnly := spec.Annotations.ReadOnlyHint != nil && *spec.Annotations.ReadOnlyHint
		if readOnly != (spec.Name == "mesh_terminal_screen" || spec.Name == "mesh_apps_list") {
			t.Errorf("%s read-only hint = %v", spec.Name, readOnly)
		}
	}
}
