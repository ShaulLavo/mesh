package bootstrap

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestDiscoverTailnetParsesAndOrdersAddresses(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "tailscale status --json" {
			t.Fatalf("command = %q", command)
		}
		return []byte("{\"BackendState\":\"Running\",\"Self\":{\"HostName\":\"pi\",\"DNSName\":\"pi.tail.example.\",\"TailscaleIPs\":[\"fd7a:115c:a1e0::8\",\"100.64.0.8\",\"100.64.0.8\"]}}"), nil, nil
	}}
	got, err := discoverTailnet(context.Background(), remote)
	if err != nil {
		t.Fatalf("discoverTailnet() error = %v", err)
	}
	if got.Name != "pi.tail.example" || !reflect.DeepEqual(got.Addresses, []string{"100.64.0.8", "fd7a:115c:a1e0::8"}) {
		t.Fatalf("discoverTailnet() = %#v", got)
	}
}

func TestDiscoverTailnetNamesLoggedOutState(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return []byte("{\"BackendState\":\"NeedsLogin\"}"), nil, nil
	}}
	_, err := discoverTailnet(context.Background(), remote)
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
}

func TestDiscoverTailnetNamesUnavailableCommand(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return nil, []byte("tailscale: not found"), errors.New("exit 127")
	}}
	_, err := discoverTailnet(context.Background(), remote)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
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
