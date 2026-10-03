package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

// All state and endpoints belong to disposable fixtures.
func TestRV164ReadyControl(t *testing.T) {
	manifest := updateTestSupportedManifest()
	target := update.Target{Host: update.Host{Alias: "local"}, State: update.Pending, Build: ptrRV164Build(updateTestBuild())}
	preview := prepareUpdateApproval(updatePreview{Release: manifest, Targets: []update.Target{target}})
	if preview.ApprovalProblem != "" {
		t.Fatal("proven transition rejected")
	}
	current := updateTestBuild()
	artifact, err := manifest.Artifact(current.Platform)
	if err != nil {
		t.Fatal(err)
	}
	current.Digest = artifact.BinarySHA256
	preview = prepareUpdateApproval(updatePreview{Release: manifest, Targets: []update.Target{{State: update.Pending, Build: &current}}})
	if !updatePreviewCurrent(preview) {
		t.Fatal("exact artifact not current")
	}
}

func ptrRV164Build(build release.Build) *release.Build { return &build }

func TestRV164CauseRetained(t *testing.T) {
	for _, scenario := range []string{"invalid digest", "missing platform", "update protocol"} {
		t.Run(scenario, func(t *testing.T) {
			build := updateTestBuild()
			switch scenario {
			case "invalid digest":
				build.Digest = "invalid"
			case "missing platform":
				build.Platform = release.Platform{OS: "windows", Arch: "amd64"}
			case "update protocol":
				build.UpdateProtocol = 0
			}
			manifest := updateTestManifest()
			cause := "unsupported update protocol"
			if build.UpdateProtocol == release.CurrentUpdateProtocol {
				cause = manifest.Allows(build).Error()
			}
			target := update.Target{Host: update.Host{Alias: "local"}, State: update.Pending, Build: &build}
			preview := prepareUpdateApproval(updatePreview{Release: manifest, Targets: []update.Target{target}})
			var machine bytes.Buffer
			if err := printUpdatePreview(&machine, preview, true, false); err != nil {
				t.Fatal(err)
			}
			t.Logf("actual underlying rejection: %s; JSON problem: %s; action: %s", cause, preview.Targets[0].Problem, preview.ApprovalProblem)
			var decoded updatePreview
			if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(preview.Targets[0].Problem, "intermediate") || !strings.Contains(decoded.Targets[0].Problem, cause) {
				t.Error("non-transition failure advertised as bridge; actual error absent from JSON diagnostics")
			}
		})
	}
}

func TestRV164InstallerEligibilityBeforeApproval(t *testing.T) {
	for _, scenario := range []string{"same version different digest", "unsupported journal"} {
		t.Run(scenario, func(t *testing.T) {
			manifest := updateTestSupportedManifest()
			build := updateTestBuild()
			if scenario == "same version different digest" {
				build.Version = manifest.Version
			} else {
				manifest.Compatibility.JournalVersion = 2
			}
			root := t.TempDir()
			engine, err := updateinstall.New(updateinstall.Config{StateDir: root, Executable: filepath.Join(root, "mesh"), CacheDir: filepath.Join(root, "cache"), ClientOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = engine.Stage(context.Background(), updateinstall.Request{ID: "review-fixture", TargetID: "fixture", Generation: 1, Manifest: manifest, Current: build})
			if err == nil {
				t.Fatal("expected real installer validation rejection")
			}
			preview := prepareUpdateApproval(updatePreview{Release: manifest, Targets: []update.Target{{State: update.Pending, Build: &build}}})
			t.Logf("real installer rejects: %s; preview approval problem = %q", err, preview.ApprovalProblem)
			if preview.ApprovalProblem == "" {
				t.Error("preview approves request real installer deterministically rejects")
			}
		})
	}
}

func TestRV164LegacyApprovalBeforeKnownBuild(t *testing.T) {
	stateDir, local := setupUpdateCLI(t)
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(_ context.Context, _ update.Host, action string, _, output any) error {
		if action != "info" {
			t.Fatal("unexpected mutation through caller")
		}
		return &update.RemoteError{Problem: `daemon: unknown control "update.control"`}
	})
	bootstraps := 0
	bootstrap := func(context.Context, updatebootstrap.Request, updatebootstrap.Config) (updateinstall.Status, error) {
		bootstraps++
		return updateinstall.Status{}, errors.New("review fixture stops before actual bootstrap")
	}
	_, _, _ = executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller, UpdateBootstrap: bootstrap}, "update", "--local", "--yes", "--json")
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy source build unavailable: %d bootstrap invocations, %d durable operations for local fixture", bootstraps, len(runs))
	if len(runs) > 0 && runs[0].Targets[0].Host.ID != local.ID {
		t.Fatal("wrong fixture target")
	}
	if bootstraps > 0 || len(runs) > 0 {
		t.Error("approval persisted and bootstrap invoked without verified source build/path")
	}
}

func TestRV164ClientOnlyApprovalBeforeBuildCheck(t *testing.T) {
	setupUpdateCLI(t)
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error { return os.ErrNotExist })
	text, err := interactiveUpdateFlow(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "n\n", "update")
	t.Logf("client-only preview offered prompt = %v, command result = %v", strings.Contains(text, "[y/N]"), err)
	current := release.Current()
	if updateTestManifest().Allows(current) == nil {
		t.Fatal("review control unexpectedly eligible")
	}
	if strings.Contains(text, "[y/N]") {
		t.Error("helper installation approval offered before checking actual ineligible CLI build")
	}
}

func TestRV164RealRPCFailureIsNotOffline(t *testing.T) {
	stateDir, local := setupUpdateCLI(t)
	_, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(stateDir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // fixture cleanup
	done := make(chan error, 1)
	go func() {
		stream, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer stream.Close() //nolint:errcheck // fixture cleanup
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			done <- err
			return
		}
		if _, err = conn.ReadFrame(); err != nil {
			done <- err
			return
		}
		payload, err := (protocol.Control{Type: "wrong-control"}).Encode()
		if err != nil {
			done <- err
			return
		}
		done <- conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}()
	targets, reviews := inspectUpdateTargets(context.Background(), update.Client{ID: local.ID, Key: key}, []update.Host{local})
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	preview := prepareUpdateApproval(updatePreview{Release: updateTestManifest(), Targets: targets, Reviews: reviews})
	t.Logf("actual update.Client protocol rejection: state=%s problem=%q approvalProblem=%q", targets[0].State, targets[0].Problem, preview.ApprovalProblem)
	if targets[0].State == update.Offline || preview.ApprovalProblem == "" {
		t.Error("genuine live fixture protocol error remains offline and passes readiness")
	}
}

func TestRV164RollbackJournalWithoutDaemon(t *testing.T) {
	_, status := localApprovalFixture(t)
	status.Phase = updateinstall.RollbackFailed
	status.Settings.ClientOnly = false
	writeApprovalJournal(t, status)
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error { return os.ErrNotExist })
	text, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err = json.Unmarshal([]byte(text), &preview); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual persisted rollback-failed journal with absent daemon: clientOnly=%v approvalProblem=%q", preview.ClientOnly, preview.ApprovalProblem)
	if !strings.Contains(strings.ToLower(preview.ApprovalProblem), "recovery") {
		t.Error("local recovery journal not consulted; failed rollback receives helper update approval instead of recovery blocker")
	}
}

func TestRV164JSONDiagnosticsControl(t *testing.T) {
	dependencies, _, _ := updateFlowFixture(t, true)
	text, _, err := executeCommand(t, dependencies, "update", "--local", "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err = json.Unmarshal([]byte(text), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Release.Commit == "" || preview.ReleaseDigest == "" || preview.Targets[0].Build.Digest == "" || preview.Targets[0].Build.UpdateProtocol == 0 {
		t.Fatal("existing technical JSON surface lost identifiers")
	}
}

func TestUpdateFlowActualTransportCauses(t *testing.T) {
	for _, scenario := range []string{"legacy authentication", "wrong identity", "unapproved device"} {
		t.Run(scenario, func(t *testing.T) {
			actor, key, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target, targetKey, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var dispatched atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler := func(_ context.Context, conn transport.Conn) error {
					if _, err := conn.ReadFrame(); err != nil {
						return fmt.Errorf("fixture read: %w", err)
					}
					dispatched.Add(1)
					return nil
				}
				if scenario == "legacy authentication" {
					_ = transport.Serve(w, r, handler)
					return
				}
				auth := &transport.Authentication{Key: targetKey, Authorize: func(string) bool { return scenario != "unapproved device" }}
				_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, handler)
			}))
			defer server.Close()
			if scenario == "wrong identity" {
				wrong, _, err := identity.LoadOrCreate(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				target.ID = wrong.ID
			}
			host := update.Host{ID: target.ID, Alias: "fixture", Endpoint: strings.Replace(server.URL, "http://", "ws://", 1)}
			targets, reviews := inspectUpdateTargets(context.Background(), update.Client{ID: actor.ID, Key: key}, []update.Host{host})
			preview := prepareUpdateApproval(updatePreview{Release: updateTestManifest(), Targets: targets, Reviews: reviews})
			if targets[0].State != update.Failed || targets[0].Build != nil || preview.ApprovalProblem == "" || dispatched.Load() != 0 {
				t.Fatalf("unsafe auth result: %+v, dispatched %d", preview, dispatched.Load())
			}
			if scenario == "legacy authentication" && reviews[host.ID].Kind != "authentication-upgrade" {
				t.Fatal("upgrade requirement lost")
			}
			if scenario != "legacy authentication" && reviews[host.ID].Kind != "authentication" {
				t.Fatal("authentication cause lost")
			}
			var output bytes.Buffer
			if err := printUpdatePreview(&output, preview, true, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), targets[0].Problem) {
				t.Fatal("original cause absent from JSON")
			}
		})
	}
}

func TestUpdateFlowUnreadableJournalBlocksAbsentDaemon(t *testing.T) {
	stateDir, _ := setupUpdateCLI(t)
	if err := os.MkdirAll(filepath.Join(stateDir, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "update", "installation.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error { return os.ErrNotExist })
	text, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--check", "--json")
	var preview updatePreview
	if err != nil || json.Unmarshal([]byte(text), &preview) != nil || preview.ApprovalProblem == "" {
		t.Fatalf("unreadable journal approved: %s, %v", text, err)
	}
	if !strings.Contains(text, "unexpected EOF") {
		t.Fatal("journal decoding cause lost")
	}
}

func TestUpdateFlowObservedWorkerEligibility(t *testing.T) {
	build := updateTestBuild()
	preview := prepareUpdateApproval(updatePreview{Release: updateTestSupportedManifest(), Targets: []update.Target{{State: update.Pending, Build: &build, Workers: []updateinstall.Worker{{ID: "fixture", Protocol: 0}}}}})
	if preview.ApprovalProblem == "" || !strings.Contains(preview.Targets[0].Problem, "incompatible or unknown worker protocol") {
		t.Fatal("unknown worker protocol approved")
	}
}

func TestUpdateFlowMalformedAuthenticatedPeer(t *testing.T) {
	for _, scenario := range []string{"certificate count", "acceptance byte"} {
		t.Run(scenario, func(t *testing.T) {
			actor, key, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target, targetKey, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, err := x509.CreateCertificate(rand.Reader, template, template, targetKey.Public(), targetKey)
			if err != nil {
				t.Fatal(err)
			}
			chain := [][]byte{der}
			if scenario == "certificate count" {
				chain = append(chain, der)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{transport.AuthProtocol}})
				if err != nil {
					return
				}
				defer ws.CloseNow() //nolint:errcheck // fixture cleanup
				secure := tls.Server(websocket.NetConn(context.WithoutCancel(r.Context()), ws, websocket.MessageBinary), &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: chain, PrivateKey: targetKey}}, ClientAuth: tls.RequireAnyClientCert})
				defer secure.Close() //nolint:errcheck // fixture cleanup
				if err := secure.HandshakeContext(r.Context()); err != nil {
					return
				}
				_, _ = secure.Write([]byte{3})
			}))
			defer server.Close()
			host := update.Host{ID: target.ID, Alias: "fixture", Endpoint: strings.Replace(server.URL, "http://", "ws://", 1)}
			targets, reviews := inspectUpdateTargets(context.Background(), update.Client{ID: actor.ID, Key: key}, []update.Host{host})
			preview := prepareUpdateApproval(updatePreview{Release: updateTestManifest(), Targets: targets, Reviews: reviews})
			cause := "invalid Mesh peer acceptance"
			if scenario == "certificate count" {
				cause = "exactly one Mesh certificate required"
			}
			if targets[0].State != update.Failed || !strings.Contains(targets[0].Problem, cause) || preview.ApprovalProblem == "" || reviews[host.ID].Kind != "authentication" {
				t.Fatalf("malformed authenticated peer became approvable: %+v", preview)
			}
			var human bytes.Buffer
			if err := printUpdatePreview(&human, preview, false, true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(human.String(), cause) {
				t.Fatal("details hid the actual authentication cause")
			}
		})
	}
}

func TestUpdateFlowCompatibleNewerAndFailedSourceMetadata(t *testing.T) {
	for _, scenario := range []string{"compatible newer", "incompatible newer", "modified newer", "malformed digest"} {
		t.Run(scenario, func(t *testing.T) {
			build := updateTestBuild()
			build.Version = "v0.3.0"
			if scenario == "incompatible newer" {
				build.StateVersion++
			}
			if scenario == "modified newer" {
				build.Modified = true
			}
			if scenario == "malformed digest" {
				build.Version = "v0.1.0"
				build.Digest = strings.Repeat("z", 64)
			}
			preview := prepareUpdateApproval(updatePreview{Release: updateTestSupportedManifest(), Targets: []update.Target{{State: update.Pending, Build: &build}}})
			if scenario == "compatible newer" {
				if preview.ApprovalProblem != "" || preview.Targets[0].State != update.Newer {
					t.Fatal("compatible newer installation not retained")
				}
				return
			}
			if preview.ApprovalProblem == "" {
				t.Fatal("ineligible metadata approved")
			}
			if scenario == "malformed digest" && preview.Reviews[""].Kind == "transition" {
				t.Fatal("malformed digest presented as missing transition")
			}
		})
	}
}
