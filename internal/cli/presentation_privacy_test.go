package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tunnel"
	"github.com/spf13/cobra"
)

func TestPrivateServiceTableMasksOnlyPresentation(t *testing.T) {
	mask := privacy.New()
	rows := []ServiceCatalogRow{{Host: HostRecord{MachineName: "private-machine", Endpoint: "wss://machine.example.ts.net/socket"}, Live: true,
		Service: protocol.ServiceInfo{Name: "private-route", DisplayName: "Secret Site", Kind: "files", Target: "/home/owner/private-project", Healthy: true}}}
	original := rows[0]
	var output bytes.Buffer
	if err := writeServiceTable(&output, rows, mask); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, private := range []string{"/home/owner/private-project", "machine.example.ts.net"} {
		if strings.Contains(text, private) {
			t.Fatalf("leaked %q: %s", private, text)
		}
	}
	for _, public := range []string{"ROUTE", "files", "tailnet", "healthy", "private-machine", "/private-route", "Secret Site", "~/private-project"} {
		if !strings.Contains(text, public) {
			t.Fatalf("missing %q: %s", public, text)
		}
	}
	if !reflect.DeepEqual(rows[0], original) {
		t.Fatal("presentation mutated service catalog")
	}
	output.Reset()
	if err := writeServiceTable(&output, rows, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "/home/owner/private-project") {
		t.Fatal("default output was masked")
	}
}

func TestPrivateServicePortsAndDiagnostics(t *testing.T) {
	mask := privacy.New()
	service := protocol.ServiceInfo{Target: "5173", Listens: []protocol.ServiceListen{{Public: 15173, Upstream: 5173}}}
	if got := privateServiceTarget(mask, service); got != serviceTargetCell(service) {
		t.Fatalf("port mapping changed: %s", got)
	}
	service.Target = "relative-secret-directory"
	if got := privateServiceTarget(mask, service); !strings.Contains(got, service.Target) || !strings.Contains(got, ":15173→5173") {
		t.Fatalf("target = %s", got)
	}
	var output bytes.Buffer
	if err := writeServiceDiagnostics(&output, map[string]error{"private-machine": errors.New("secret-token-without-recognizable-format")}, mask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "private-machine") || strings.Contains(output.String(), "secret-token") || !strings.Contains(output.String(), "unavailable") {
		t.Fatalf("diagnostics = %s", output.String())
	}
	rows := []ServiceCatalogRow{{Host: HostRecord{ID: "id", MachineName: "private-machine"}, Service: protocol.ServiceInfo{Name: "private-route", PublicName: "secret.example"}}, {Host: HostRecord{ID: "id", MachineName: "private-machine"}, Service: protocol.ServiceInfo{Name: "private-route/admin"}}}
	output.Reset()
	if err := writeServiceShadowWarnings(&output, rows, mask); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-route") || strings.Contains(output.String(), "secret.example") || !strings.Contains(output.String(), "[details withheld]") {
		t.Fatalf("shadow warning = %s", output.String())
	}
	if got := privateServiceCommand(mask, "echo arbitrary-secret"); strings.Contains(got, "secret") {
		t.Fatalf("command leaked: %s", got)
	}
}

func TestPrivateAppPresentationWithholdsPayloads(t *testing.T) {
	mask := privacy.New()
	app := appspkg.Record{ID: "7k3d", Owner: "private-owner", Status: "active"}
	result := appspkg.Result{App: &app, Runtime: &appspkg.RuntimeInfo{Problem: "unrecognizable-secret", Failure: &appspkg.SetupFailure{Error: "secret-error", Output: "secret-build-output"}}}
	var output bytes.Buffer
	if err := writeAppResult(&output, "private-machine", result, false, mask); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-owner", "unrecognizable-secret", "secret-error", "secret-build-output"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("leaked %q: %s", private, output.String())
		}
	}
	for _, public := range []string{"active", "private", "setup failed", "not serving", "private-machine", "id: 7k3d", mask.Value("url", appspkg.URL(app.ID)), mask.Value("owner", app.Owner)} {
		if !strings.Contains(output.String(), public) {
			t.Fatalf("missing %q: %s", public, output.String())
		}
	}
	if result.App.Owner != "private-owner" || result.Runtime.Failure.Output != "secret-build-output" {
		t.Fatal("presentation mutated authoritative app")
	}
	output.Reset()
	result = appspkg.Result{Browsers: json.RawMessage(`[{"token":"unrecognizable-secret"}]`)}
	if err := writeAppResult(&output, "host", result, false, mask); err != nil {
		t.Fatal(err)
	}
	if output.String() != "[browser details withheld]\n" {
		t.Fatalf("browser output = %s", output.String())
	}
	output.Reset()
	result = appspkg.Result{Apps: []appspkg.Record{app}}
	if err := writeAppResult(&output, "host", result, false, mask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), app.ID) || !strings.Contains(output.String(), "active") {
		t.Fatalf("app list = %s", output.String())
	}
	output.Reset()
	if err := writeAppResult(&output, "host", result, true, mask); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		appspkg.Result
		Host string
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Host != mask.Value("host", "host") || decoded.Apps[0].ID != mask.Value("app", app.ID) {
		t.Fatalf("private JSON = %s", output.String())
	}

}

func TestPrivateRecoveryOutputDoesNotReadArchive(t *testing.T) {
	a := &application{privacy: privacy.New()}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	// An invalid ID proves privacy does not even resolve or read the archive.
	if err := a.previousOutput(cmd, "invalid-sensitive-session", 1024); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Previous output withheld in privacy mode\n" {
		t.Fatalf("previous output = %q", output.String())
	}
}

func TestPrivateAppJSONMutationUsesOriginalControlData(t *testing.T) {
	called := false
	output, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, host string, request appspkg.Request) (appspkg.Result, error) {
		called = true
		if host != "private-machine" || request.ID != "7k3d" || request.Action != "renew" {
			t.Fatalf("masked control data: %s %+v", host, request)
		}
		return appspkg.Result{App: &appspkg.Record{ID: "7k3d", Owner: "private-owner", Status: "active"}}, nil
	}}, "--privacy", "app", "renew", "private-machine", "7k3d", "--json")
	if err != nil || !called {
		t.Fatalf("mutation = %v, called %t", err, called)
	}
	if strings.Contains(output, "private-owner") || !strings.Contains(output, "private-machine") || !strings.Contains(output, "7k3d") {
		t.Fatalf("JSON leaked: %s", output)
	}
	var result struct {
		appspkg.Result
		Host, URL string
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.App.Status != "active" || result.URL != privacy.New().Value("url", "https://7k3d.mesh.test") {
		t.Fatalf("result shape changed: %s", output)
	}
}

func TestPrivateAppJSONClonesEveryPrivatePayload(t *testing.T) {
	mask := privacy.New()
	result := appspkg.Result{App: &appspkg.Record{ID: "7k3d", Owner: "secret-owner", Revision: "secret-revision"}, Runtime: &appspkg.RuntimeInfo{SessionID: "7K3D", Root: "/home/private-person/project", Command: "secret-command", Problem: "secret-problem", Port: 5173, Failure: &appspkg.SetupFailure{UploadID: "secret-upload", Error: "secret-error", Output: "secret-output"}}, UploadID: "secret-upload", Data: []byte("secret-data"), Browsers: json.RawMessage(`[{"secret-key":"secret-browser"}]`)}
	before, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeAppResult(&output, "secret-host", result, true, mask); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		appspkg.Result
		Host, URL string
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("JSON masking mutated result")
	}
	for _, private := range []string{"secret-owner", "secret-revision", "secret-upload", "secret-command", "secret-problem", "secret-error", "secret-output", "private-person", "secret-browser", "secret-key"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("payload leaked %q: %s", private, output.String())
		}
	}
	if decoded.Host != "secret-host" || decoded.App.ID != "7k3d" || decoded.Runtime.SessionID != "7K3D" || decoded.Runtime.Root != "~/project" {
		t.Fatalf("useful identifiers were not retained: %s", output.String())
	}
	if strings.Contains(string(decoded.Data), "secret-data") {
		t.Fatalf("binary payload leaked: %s", decoded.Data)
	}
	if decoded.Runtime.Port != 5173 || string(decoded.Browsers) != "null" || decoded.Runtime.Failure.Output != "[build output withheld]" {
		t.Fatalf("projection = %s", output.String())
	}
}

func TestPrivateServiceLabelPreservesAction(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{{Name: "private-route", Kind: "proxy", Target: "5173", Healthy: true}}
	output, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "--privacy", "serve", "label", "/private-route", "Secret Site", "--host", "pc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "/private-route") || !strings.Contains(output, "Secret Site") || !strings.Contains(output, " on pc") {
		t.Fatalf("useful label presentation lost: %s", output)
	}
	if host.services[0].Name != "private-route" || host.services[0].DisplayName != "Secret Site" {
		t.Fatalf("label control data changed: %+v", host.services)
	}
}

func TestPrivateTunnelClaimAndReleasePreserveSignedNames(t *testing.T) {
	host, _, stateDir := setupTunnelCLI(t)
	mask := privacy.New()
	var actions []tunnel.Action
	deps := Dependencies{DialControl: serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.Type != protocol.TypeTunnelClaim || request.TunnelMutation == nil {
			t.Fatalf("unexpected control: %+v", request)
		}
		mutation := *request.TunnelMutation
		if mutation.PublicName != "secret.mesh.test" {
			t.Fatalf("masked signed hostname: %+v", mutation)
		}
		digest, err := tunnel.Verify(mutation, host.ID)
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, mutation.Action)
		return protocol.Control{Type: protocol.TypeTunnelClaimed, TunnelAck: &tunnel.Ack{Sequence: mutation.Sequence, Digest: digest}}
	})}
	output, _, err := executeCommand(t, deps, "--privacy", "serve", "claim", "vps", "secret.mesh.test", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, host.TailscaleName) {
		t.Fatalf("claim leaked private DNS: %s", output)
	}
	for _, value := range []string{"secret.mesh.test", " on vps", mask.Value("path", stateDir+"/identity.key"), mask.Value("host", host.TailscaleName)} {
		if !strings.Contains(output, value) {
			t.Fatalf("claim lost usable summary %q: %s", value, output)
		}
	}
	if !strings.Contains(output, "ssh -N") || !strings.Contains(output, "2222") {
		t.Fatalf("claim summary lost safe details: %s", output)
	}
	output, _, err = executeCommand(t, deps, "--privacy", "unserve", "secret.mesh.test", "--host", "vps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "secret.mesh.test") || !strings.Contains(output, " on vps") {
		t.Fatalf("release lost useful names: %s", output)
	}
	if len(actions) != 2 || actions[0] != tunnel.Create || actions[1] != tunnel.Release {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestPrivatePublicConfirmationRetainsUsefulTarget(t *testing.T) {
	mask := privacy.New()
	confirmation := PublicConfirmation{
		Host:      HostRecord{MachineName: "recording-owner@pc"},
		Service:   protocol.ServiceInfo{Kind: "files", Target: "/home/private-person/site"},
		FileCount: 3,
		URL:       "https://pc.example.ts.net/site/page?view=owner",
	}
	var output bytes.Buffer
	if err := writePublicConfirmation(&output, confirmation, mask); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"recording-owner", "private-person", "example.ts.net"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("confirmation leaked %q: %s", private, output.String())
		}
	}
	for _, useful := range []string{"pc", "~/site", "Files: 3", "https://", "/site/page", "Continue? [y/N]"} {
		if !strings.Contains(output.String(), useful) {
			t.Fatalf("confirmation lost %q: %s", useful, output.String())
		}
	}
	if confirmation.Service.Target != "/home/private-person/site" || confirmation.Host.MachineName != "recording-owner@pc" {
		t.Fatal("confirmation mutated original facts")
	}
}
