package bootstrap

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDiscoverTailnetParsesAndOrdersAddresses(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != tailscaleStatusCommand || !strings.Contains(command, tailscaleApplicationCLI) {
			t.Fatalf("command = %q", command)
		}
		return []byte("{\"BackendState\":\"Running\",\"Self\":{\"HostName\":\"pi\",\"DNSName\":\"pi.tail.example.\",\"TailscaleIPs\":[\"fd7a:115c:a1e0::8\",\"100.64.0.8\",\"100.64.0.8\"]}}"), nil, nil
	}}
	got, err := discoverTailnet(context.Background(), remote)
	if err != nil {
		t.Fatalf("discoverTailnet() error = %v", err)
	}
	if got.State != tailscaleRunning || got.Tailnet.Name != "pi.tail.example" || !reflect.DeepEqual(got.Tailnet.Addresses, []string{"100.64.0.8", "fd7a:115c:a1e0::8"}) {
		t.Fatalf("discoverTailnet() = %#v", got)
	}
}

func TestRunningTailnetRejectsControlCharactersInName(t *testing.T) {
	t.Parallel()

	_, err := parseRunningTailnet(tailscaleStatus{Self: &tailscalePeer{
		DNSName: "pi.tail.example\nforged-output", TailscaleIPs: []string{"100.64.0.8"},
	}})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
}

func TestDiscoverTailnetIdentifiesExistingMacApplication(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return []byte(`{"BackendState":"Running","Self":{"HostName":"mac","TailscaleIPs":["100.64.0.9"]}}`), []byte("MESH_TAILSCALE_VARIANT=application\nMESH_TAILSCALE_CLI=/Applications/Tailscale.app/Contents/MacOS/Tailscale\n"), nil
	}}
	got, err := discoverTailnet(context.Background(), remote)
	if err != nil || got.Variant != tailscaleVariantApplication || got.Variant.detail() != "Tailscale application" {
		t.Fatalf("discoverTailnet() = %#v, %v", got, err)
	}
}

func TestTailscaleStatusProbeChecksHomebrewCLILocations(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale"} {
		if !strings.Contains(tailscaleStatusCommand, path) {
			t.Fatalf("Tailscale status probe does not check %s: %q", path, tailscaleStatusCommand)
		}
	}
}

func TestTailscaleStatusProbeRejectsBrokenMacApplicationBundle(t *testing.T) {
	t.Parallel()

	if !strings.Contains(tailscaleStatusCommand, "[ -d '/Applications/Tailscale.app' ]") || !strings.Contains(tailscaleStatusCommand, "MESH_TAILSCALE_APPLICATION_BROKEN=yes") {
		t.Fatalf("Tailscale status probe can fall through a broken application bundle: %q", tailscaleStatusCommand)
	}
	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return nil, []byte("MESH_TAILSCALE_APPLICATION_BROKEN=yes\n"), errors.New("exit 126")
	}}
	got, err := discoverTailnet(context.Background(), remote)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if got.State == tailscaleMissing || !strings.Contains(err.Error(), "bundled CLI") {
		t.Fatalf("discoverTailnet() = %#v, %v", got, err)
	}
}

func TestDiscoverTailnetClassifiesProvisionableStates(t *testing.T) {
	t.Parallel()

	for backendState, want := range map[string]tailscaleState{
		"NeedsLogin":       tailscaleNeedsLogin,
		"NoState":          tailscaleNoState,
		"Stopped":          tailscaleStopped,
		"NeedsMachineAuth": tailscaleNeedsMachineAuth,
	} {
		t.Run(backendState, func(t *testing.T) {
			remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
				return []byte("{\"BackendState\":" + strconv.Quote(backendState) + "}"), nil, nil
			}}
			got, err := discoverTailnet(context.Background(), remote)
			if err != nil || got.State != want {
				t.Fatalf("discoverTailnet() = %#v, %v, want state %s", got, err, want)
			}
		})
	}
}

func TestDiscoverTailnetClassifiesUnavailableCommand(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return nil, []byte("MESH_TAILSCALE_MISSING=yes\n"), errors.New("exit 127")
	}}
	got, err := discoverTailnet(context.Background(), remote)
	if err != nil || got.State != tailscaleMissing {
		t.Fatalf("discoverTailnet() = %#v, %v, want Missing", got, err)
	}
}

func TestDiscoverTailnetRejectsMissingMarkerFromSuccessfulProbe(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return nil, []byte("MESH_TAILSCALE_MISSING=yes\n"), nil
	}}
	_, err := discoverTailnet(context.Background(), remote)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
}

func TestTailscaleProbeRequiresPathForKnownVariant(t *testing.T) {
	t.Parallel()

	_, _, _, err := parseTailscaleProbe([]byte("MESH_TAILSCALE_VARIANT=system\n"))
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
}

func TestDiscoverTailnetDoesNotTreatArbitraryNotFoundTextAsMissing(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return nil, []byte("profile not found"), errors.New("exit 1")
	}}
	_, err := discoverTailnet(context.Background(), remote)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
}

func TestDiscoverTailnetPollsStartingToRunning(t *testing.T) {
	calls := 0
	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		calls++
		if calls < 3 {
			return []byte(`{"BackendState":"Starting"}`), nil, nil
		}
		return []byte(`{"BackendState":"Running","Self":{"HostName":"pi","TailscaleIPs":["100.64.0.8"]}}`), nil, nil
	}}
	got, err := discoverTailnetWithTiming(context.Background(), remote, time.Second, time.Nanosecond)
	if err != nil || got.State != tailscaleRunning || calls != 3 {
		t.Fatalf("discoverTailnetWithTiming() = %#v, %v after %d calls", got, err, calls)
	}
}

func TestDiscoverTailnetBoundsStartingPoll(t *testing.T) {
	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return []byte(`{"BackendState":"Starting"}`), nil, nil
	}}
	_, err := discoverTailnetWithTiming(context.Background(), remote, 5*time.Millisecond, time.Millisecond)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if !strings.Contains(err.Error(), "Starting") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("starting deadline error = %v", err)
	}
}

func TestCheckRemoteClockNamesSkew(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return []byte("100\n"), nil, nil
	}}
	err := checkRemoteClock(context.Background(), remote, time.Unix(100, 0).Add(maximumClockSkew+time.Second))
	assertDiagnosticCode(t, err, DiagnosticClockSkew)
}

func assertDiagnosticCode(t *testing.T, err error, want DiagnosticCode) {
	t.Helper()
	var diagnosticError *DiagnosticError
	if !errors.As(err, &diagnosticError) || diagnosticError.Code != want {
		t.Fatalf("error = %v, want diagnostic %s", err, want)
	}
}
