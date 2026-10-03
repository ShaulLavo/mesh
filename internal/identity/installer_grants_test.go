package identity

import (
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	installscript "github.com/shaul/mesh/scripts/install"
	"golang.org/x/crypto/ssh"
)

func installerGrantFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "../.."))
	home, err := os.MkdirTemp(t.TempDir(), "installer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	state := filepath.Join(home, ".local/state/mesh")
	bin := filepath.Join(home, "fake-bin")
	for _, dir := range []string{state, bin, filepath.Join(home, ".local/bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(t.TempDir(), "mesh")
	build := exec.Command("go", "build", "-o", candidate, "./cmd/mesh") //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	build.Dir = root                                                    //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture binary: %v %s", err, out)
	}
	contents, err := os.ReadFile(candidate) //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local/bin/mesh"), contents, 0700); err != nil { //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
		t.Fatal(err)
	}
	for name, content := range map[string]string{"loginctl": "#!/bin/sh\necho Linger=yes\n", "systemctl": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil { //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
			t.Fatal(err)
		}
	}
	script, _ := installscript.Script("linux")
	return home, state, bin, script + "\n"
}

func installerGrantCommand(t *testing.T, home, bin, script string, key ssh.PublicKey) *exec.Cmd {
	t.Helper()
	service, err := installscript.RenderService("linux", installscript.ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-s", "--", "", "7337", "2222", "/mesh", base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))), base64.StdEncoding.EncodeToString([]byte(service))) //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	return cmd
}

func TestBootstrapReapprovalMustNotHealBareGrant(t *testing.T) {
	home, state, bin, script := installerGrantFixture(t)
	actor, key, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "authorized_keys"), ssh.MarshalAuthorizedKey(pub), 0600); err != nil { //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
		t.Fatal(err)
	}
	prior, ok := BindIdentity(state, actor.ID)
	if !ok {
		t.Fatal("fixture grant missing")
	}
	if err := RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	cmd := installerGrantCommand(t, home, bin, script, pub)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installer: %v %s", err, out)
	}
	if prior() {
		t.Fatal("mesh add installer reapproval healed original bare-key grant incarnation")
	}
}

func TestBootstrapMustSerializeWithDeviceRevocation(t *testing.T) {
	home, state, bin, script := installerGrantFixture(t)
	first, key, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := ApproveDevice(state, id); err != nil {
			t.Fatal(err)
		}
	}
	prior, ok := BindIdentity(state, second.ID)
	if !ok {
		t.Fatal("fixture grant missing")
	}
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(home, "ready")
	proceed := filepath.Join(home, "proceed")
	for _, path := range []string{ready, proceed} {
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	awkTool, err := exec.LookPath("awk")
	if err != nil {
		t.Skipf("installer fixture needs awk: %v", err)
	}
	awk := "#!/bin/sh\n\"" + awkTool + "\" \"$@\"\nprintf 'r' >\"$HOME/ready\"\nread -r resumed <\"$HOME/proceed\"\n"
	if err := os.WriteFile(filepath.Join(bin, "awk"), []byte(awk), 0700); err != nil { //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
		t.Fatal(err)
	}
	cmd := installerGrantCommand(t, home, bin, script, pub)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	snapshot, err := os.Open(ready) //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	if err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err := io.ReadFull(snapshot, signal[:]); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RevokeDevice(state, second.ID); err != nil {
		t.Fatal(err)
	}
	resume, err := os.OpenFile(proceed, os.O_WRONLY, 0600) //nolint:gosec // owned fixture paths, executable payloads and checkout-derived build arguments
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resume.Write([]byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	if err := resume.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if prior() || GrantedIdentity(state, second.ID) {
		t.Fatal("concurrent mesh add installer restored successfully revoked second device and its original incarnation")
	}
}
