package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	installscript "github.com/shaul/mesh/scripts/install"
)

func TestInstallRemoteStreamsBinaryAndReportsUnchanged(t *testing.T) {
	t.Parallel()

	binaryPath := writeTestBinary(t, []byte("binary contents"))
	call := 0
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		call++
		switch call {
		case 1:
			return nil, nil, nil // no mesh binary installed yet
		case 2:
			return []byte("/tmp/mesh-bootstrap.ABC123\n"), nil, nil
		case 3:
			contents, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "binary contents" || !strings.Contains(command, "cat >") {
				t.Fatalf("upload command = %q, contents = %q", command, contents)
			}
			return nil, nil, nil
		case 4:
			script, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			service, err := installscript.RenderService("linux", installscript.ServiceOptions{
				DaemonPort:    7337,
				SSHPort:       2222,
				WebSocketPath: "/mesh",
			})
			if err != nil {
				t.Fatal(err)
			}
			encodedService := base64.StdEncoding.EncodeToString([]byte(service))
			if !bytes.Contains(script, []byte("systemctl --user")) || !strings.Contains(command, "'"+encodedService+"'") {
				t.Fatalf("installer command = %q", command)
			}
			return []byte("MESH_INSTALL_RESULT=unchanged\n"), nil, nil
		case 5:
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
		SSHPort:       2222,
		WebSocketPath: "/mesh",
	})
	if err != nil || !unchanged {
		t.Fatalf("installRemote() unchanged = %v, error = %v", unchanged, err)
	}
}

func TestInstallRemoteStreamsCanonicalLaunchdService(t *testing.T) {
	t.Parallel()

	binaryPath := writeTestBinary(t, []byte("darwin binary"))
	service, err := installscript.RenderService("darwin", installscript.ServiceOptions{
		DaemonPort:    7337,
		SSHPort:       2222,
		WebSocketPath: "/mesh",
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedService := base64.StdEncoding.EncodeToString([]byte(service))
	call := 0
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		call++
		switch call {
		case 1:
			return nil, nil, nil // no mesh binary installed yet
		case 2:
			return []byte("/tmp/mesh-bootstrap.DARWIN\n"), nil, nil
		case 3:
			_, err := io.Copy(io.Discard, stdin)
			return nil, nil, err
		case 4:
			script, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(script, []byte("launchctl bootstrap")) || !strings.Contains(command, "'"+encodedService+"'") {
				t.Fatalf("installer command = %q", command)
			}
			return []byte("MESH_INSTALL_RESULT=configured\n"), nil, nil
		case 5:
			return nil, nil, nil
		default:
			t.Fatalf("unexpected call %d: %q", call, command)
			return nil, nil, nil
		}
	}}
	unchanged, err := installRemote(context.Background(), remote, installRequest{
		Platform:      Platform{OS: Darwin, Arch: ARM64},
		BinaryPath:    binaryPath,
		AuthorizedKey: "ssh-ed25519 adopter",
		DaemonPort:    7337,
		SSHPort:       2222,
		WebSocketPath: "/mesh",
	})
	if err != nil || unchanged {
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
					return nil, nil, nil // no mesh binary installed yet
				case 2:
					return []byte("/tmp/mesh-bootstrap.ABC123\n"), nil, nil
				case 3:
					return nil, nil, nil
				case 4:
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
				SSHPort:       2222,
				WebSocketPath: "/mesh",
			})
			assertDiagnosticCode(t, err, code)
		})
	}
}

func writeTestBinary(t *testing.T, contents []byte) string {
	t.Helper()
	path := t.TempDir() + "/mesh"
	if err := os.WriteFile(path, contents, 0o700); err != nil { //nolint:gosec // executable fixture requires owner execute permission
		t.Fatal(err)
	}
	return path
}

func TestInstallRemoteSkipsUploadWhenHostRunsThisBuild(t *testing.T) {
	t.Parallel()

	contents := []byte("identical binary")
	binaryPath := writeTestBinary(t, contents)
	digest := sha256.Sum256(contents)

	var commands []string
	var reported []Event
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		commands = append(commands, command)
		if len(commands) == 1 {
			return []byte(hex.EncodeToString(digest[:]) + "\n"), nil, nil
		}
		if _, err := io.Copy(io.Discard, stdin); err != nil {
			t.Fatal(err)
		}
		return []byte("MESH_INSTALL_RESULT=unchanged\n"), nil, nil
	}}
	unchanged, err := installRemote(context.Background(), remote, installRequest{
		Platform:      Platform{OS: Linux, Arch: AMD64},
		BinaryPath:    binaryPath,
		AuthorizedKey: "ssh-ed25519 adopter",
		DaemonPort:    7337,
		SSHPort:       2222,
		WebSocketPath: "/mesh",
		Progress:      func(event Event) { reported = append(reported, event) },
	})
	if err != nil || !unchanged {
		t.Fatalf("installRemote() unchanged = %v, error = %v", unchanged, err)
	}

	// The probe and the installer. No mktemp, no upload, no cleanup: adoption
	// still reconciles the service and the keys without resending the binary.
	if len(commands) != 2 {
		t.Fatalf("ran %d commands, want the probe and the installer: %q", len(commands), commands)
	}
	if strings.Contains(commands[1], "mktemp") || strings.Contains(commands[1], "cat >") {
		t.Fatalf("installer command still uploads: %q", commands[1])
	}
	// The empty first argument is what tells the installer to keep its binary.
	if !strings.Contains(commands[1], "/bin/sh -s -- '' ") {
		t.Fatalf("installer was not told the upload was skipped: %q", commands[1])
	}
	if len(reported) != 1 || reported[0].Step != StepTransfer {
		t.Fatalf("progress = %+v, want one transfer event saying the upload was skipped", reported)
	}
}

func TestInstallRemoteUploadsWhenTheDigestIsUnusable(t *testing.T) {
	t.Parallel()

	// Silence, a truncated digest, a different digest, and a probe that fails
	// all mean the same thing: we do not know, so we send the binary. Reading
	// any of these as a match would leave the host on a stale build.
	contents := []byte("staged binary")
	digest := sha256.Sum256(contents)
	other := sha256.Sum256([]byte("some older build"))
	for name, probe := range map[string]func() ([]byte, []byte, error){
		"no binary":       func() ([]byte, []byte, error) { return nil, nil, nil },
		"no hasher":       func() ([]byte, []byte, error) { return []byte("\n"), nil, nil },
		"truncated":       func() ([]byte, []byte, error) { return []byte(hex.EncodeToString(digest[:])[:32]), nil, nil },
		"different build": func() ([]byte, []byte, error) { return []byte(hex.EncodeToString(other[:])), nil, nil },
		"probe failed":    func() ([]byte, []byte, error) { return nil, nil, errors.New("exit 127") },
	} {
		t.Run(name, func(t *testing.T) {
			binaryPath := writeTestBinary(t, contents)
			uploaded := false
			call := 0
			remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
				call++
				switch call {
				case 1:
					return probe()
				case 2:
					return []byte("/tmp/mesh-bootstrap.ABC123\n"), nil, nil
				case 3:
					body, err := io.ReadAll(stdin)
					if err != nil {
						t.Fatal(err)
					}
					uploaded = bytes.Equal(body, contents)
					return nil, nil, nil
				case 4:
					if _, err := io.Copy(io.Discard, stdin); err != nil {
						t.Fatal(err)
					}
					return []byte("MESH_INSTALL_RESULT=configured\n"), nil, nil
				default:
					return nil, nil, nil
				}
			}}
			if _, err := installRemote(context.Background(), remote, installRequest{
				Platform:      Platform{OS: Linux, Arch: AMD64},
				BinaryPath:    binaryPath,
				AuthorizedKey: "ssh-ed25519 adopter",
				DaemonPort:    7337,
				SSHPort:       2222,
				WebSocketPath: "/mesh",
			}); err != nil {
				t.Fatal(err)
			}
			if !uploaded {
				t.Fatal("installRemote() skipped the upload on an unusable digest")
			}
		})
	}
}
