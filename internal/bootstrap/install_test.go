package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestInstallRemoteStreamsBinaryAndReportsUnchanged(t *testing.T) {
	t.Parallel()

	binaryPath := writeTestBinary(t, []byte("binary contents"))
	call := 0
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		call++
		switch call {
		case 1:
			return []byte("/tmp/mesh-bootstrap.ABC123\n"), nil, nil
		case 2:
			contents, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "binary contents" || !strings.Contains(command, "cat >") {
				t.Fatalf("upload command = %q, contents = %q", command, contents)
			}
			return nil, nil, nil
		case 3:
			script, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(script, []byte("systemctl --user")) || !strings.Contains(command, "7337") {
				t.Fatalf("installer command = %q", command)
			}
			return []byte("MESH_INSTALL_RESULT=unchanged\n"), nil, nil
		case 4:
			if !strings.HasPrefix(command, "rm -f -- '/tmp/mesh-bootstrap.") {
				t.Fatalf("cleanup command = %q", command)
			}
			return nil, nil, nil
		default:
			t.Fatalf("unexpected call %d: %q", call, command)
			return nil, nil, nil
		}
	}}
	unchanged, err := installRemote(context.Background(), remote, installRequest{
		Platform:      Platform{OS: Linux, Arch: AMD64},
		BinaryPath:    binaryPath,
		AuthorizedKey: "ssh-ed25519 adopter",
		DaemonPort:    7337,
		WebSocketPath: "/mesh",
	})
	if err != nil || !unchanged {
		t.Fatalf("installRemote() unchanged = %v, error = %v", unchanged, err)
	}
}

func TestInstallRemoteMapsInstallerFailures(t *testing.T) {
	t.Parallel()

	for _, code := range []DiagnosticCode{DiagnosticNoSystemd, DiagnosticNoUserLingering, DiagnosticServiceInstall} {
		t.Run(string(code), func(t *testing.T) {
			binaryPath := writeTestBinary(t, []byte("binary"))
			call := 0
			remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
				call++
				switch call {
				case 1:
					return []byte("/tmp/mesh-bootstrap.ABC123\n"), nil, nil
				case 2:
					return nil, nil, nil
				case 3:
					return nil, []byte("MESH_BOOTSTRAP_ERROR=" + string(code) + "\nfixture\n"), errors.New("exit 1")
				default:
					return nil, nil, nil
				}
			}}
			_, err := installRemote(context.Background(), remote, installRequest{
				Platform:      Platform{OS: Linux, Arch: AMD64},
				BinaryPath:    binaryPath,
				AuthorizedKey: "ssh-ed25519 adopter",
				DaemonPort:    7337,
				WebSocketPath: "/mesh",
			})
			assertDiagnosticCode(t, err, code)
		})
	}
}

func writeTestBinary(t *testing.T, contents []byte) string {
	t.Helper()
	path := t.TempDir() + "/mesh"
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
