package dnsname

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func bindingTestDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func bindingSyncPaths(path string) []string {
	paths := []string{"file"}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		paths = append(paths, dir)
		if dir == filepath.Dir(dir) {
			return paths
		}
	}
}

func TestCredentialBindingDurabilityFailuresAndRetry(t *testing.T) {
	// Every directory is re-synced on retry: existence does not prove that an
	// earlier initializer made its directory entries durable.
	root := bindingTestDirectory(t)
	probe := renewalCredentialBindingPath("/config", filepath.Join(root, "new", "state"))
	for _, scenario := range []string{"first", "identical", "rebind"} {
		for step := range bindingSyncPaths(probe) {
			t.Run(fmt.Sprintf("%s/%d", scenario, step), func(t *testing.T) {
				state := filepath.Join(bindingTestDirectory(t), "new", "state")
				path := renewalCredentialBindingPath("/config", state)
				expected := bindingSyncPaths(path)
				config := PrivateNamesConfig{Domain: "sprockt.dev", ZoneID: "zone"}
				if scenario != "first" {
					if err := bindRenewalCredentials("/config", state, config); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "rebind" {
					config.Domain = "new.example"
					config.ZoneDomain = config.Domain
				}
				injected := errors.New("injected fsync failure")
				var seen []string
				syncFile := func(f *os.File) error {
					info, err := f.Stat()
					if err != nil {
						return fmt.Errorf("inspect sync target: %w", err)
					}
					name := "file"
					if info.IsDir() {
						name = f.Name()
					}
					seen = append(seen, name)
					if len(seen)-1 == step {
						return injected
					}
					return f.Sync()
				}
				if err := bindRenewalCredentialsWithSync("/config", state, config, syncFile); !errors.Is(err, injected) {
					t.Fatalf("initializer accepted failed durability step %d: %v (synced %v)", step, err, seen)
				}
				if !reflect.DeepEqual(seen, expected[:step+1]) {
					t.Fatalf("sync order %v, want %v", seen, expected[:step+1])
				}
				lockBefore, err := os.Stat(path + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				seen = nil
				repair := func(f *os.File) error {
					info, err := f.Stat()
					if err != nil {
						return fmt.Errorf("inspect sync target: %w", err)
					}
					name := "file"
					if info.IsDir() {
						name = f.Name()
					}
					seen = append(seen, name)
					return f.Sync()
				}
				if err := bindRenewalCredentialsWithSync("/config", state, config, repair); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(seen, expected) {
					t.Fatalf("retry did not repair full barrier: %v, want %v", seen, expected)
				}
				lockAfter, err := os.Stat(path + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(lockBefore, lockAfter) {
					t.Fatal("retry replaced stable lock inode")
				}
				seen = nil
				if err := bindRenewalCredentialsWithSync("/config", state, config, repair); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(seen, expected) {
					t.Fatalf("identical reader skipped barrier: %v", seen)
				}
			})
		}
	}
}

func TestCredentialBindingProcessHelper(t *testing.T) {
	state := os.Getenv("MESH_BINDING_TEST_STATE")
	if state == "" {
		return
	}
	if os.Getenv("MESH_BINDING_TEST_UMASK") == "restrictive" {
		unix.Umask(0o777)
	}
	fmt.Println("started")
	err := bindRenewalCredentials("/config", state, PrivateNamesConfig{Domain: os.Getenv("MESH_BINDING_TEST_DOMAIN"), ZoneID: "zone"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	fmt.Println("accepted")
}

func bindingProcess(t *testing.T, state, domain string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCredentialBindingProcessHelper$") //nolint:gosec // run only this test executable with a fixed helper selector
	cmd.Env = append(os.Environ(), "MESH_BINDING_TEST_STATE="+state, "MESH_BINDING_TEST_DOMAIN="+domain)
	return cmd
}

func TestCredentialBindingReaderWaitsForDurablePublication(t *testing.T) {
	for _, barrier := range []string{"file", "leaf", "parent", "root"} {
		t.Run(barrier, func(t *testing.T) {
			state := filepath.Join(bindingTestDirectory(t), "new", "state")
			leaf := filepath.Dir(renewalCredentialBindingPath("/config", state))
			if barrier == "parent" {
				leaf = filepath.Dir(leaf)
			}
			if barrier == "root" {
				leaf = string(filepath.Separator)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			result := make(chan error, 1)
			go func() {
				result <- bindRenewalCredentialsWithSync("/config", state, PrivateNamesConfig{Domain: "sprockt.dev", ZoneID: "zone"}, func(f *os.File) error {
					info, err := f.Stat()
					if err != nil {
						return fmt.Errorf("inspect gated sync target: %w", err)
					}
					if (barrier == "file" && !info.IsDir()) || (barrier != "file" && f.Name() == leaf) {
						close(reached)
						<-release
					}
					return f.Sync()
				})
			}()
			select {
			case <-reached:
			case err := <-result:
				t.Fatalf("publisher skipped gated directory fsync: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("publisher did not reach barrier")
			}
			cmd := bindingProcess(t, state, "sprockt.dev")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "started" {
				t.Fatal("reader did not start")
			}
			done := make(chan error, 1)
			accepted := make(chan struct{}, 1)
			go func() {
				for scanner.Scan() {
					if scanner.Text() == "accepted" {
						accepted <- struct{}{}
					}
				}
				done <- cmd.Wait()
			}()
			select {
			case <-accepted:
				t.Fatal("cross-process reader accepted before durability barrier")
			case err := <-done:
				t.Fatalf("cross-process reader returned before durability barrier: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			release <- struct{}{}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reader stayed blocked")
			}
		})
	}
}

func TestCredentialBindingConcurrentProcessInitializers(t *testing.T) {
	state := filepath.Join(bindingTestDirectory(t), "new", "state")
	commands := []*exec.Cmd{bindingProcess(t, state, "sprockt.dev"), bindingProcess(t, state, "old.example")}
	for _, cmd := range commands {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	accepted := 0
	for _, cmd := range commands {
		if err := cmd.Wait(); err == nil {
			accepted++
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatal(err)
			}
		}
	}
	if accepted != 1 {
		t.Fatalf("competing processes accepted %d bindings", accepted)
	}
	entries, err := os.ReadDir(filepath.Dir(renewalCredentialBindingPath("/config", state)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".binding-") {
			t.Fatalf("temporary binding remains: %s", entry.Name())
		}
	}
}

func TestCredentialBindingSymlinkedStateAncestors(t *testing.T) {
	root := bindingTestDirectory(t)
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(alias, "new", "state")
	var synced []string
	err := bindRenewalCredentialsWithSync("/config", state, PrivateNamesConfig{Domain: "sprockt.dev", ZoneID: "zone"}, func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect sync target: %w", err)
		}
		if info.IsDir() {
			synced = append(synced, file.Name())
		}
		return file.Sync()
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	expected := bindingSyncPaths(renewalCredentialBindingPath("/config", filepath.Join(canonical, "new", "state")))[1:]
	if !reflect.DeepEqual(synced, expected) {
		t.Fatalf("did not sync physical ancestors: %v, want %v", synced, expected)
	}
}

func TestCredentialBindingPublicationModeWithRestrictiveUmask(t *testing.T) {
	state := bindingTestDirectory(t)
	config := PrivateNamesConfig{Domain: "sprockt.dev", ZoneID: "zone"}
	if err := bindRenewalCredentials("/config", state, config); err != nil {
		t.Fatal(err)
	}
	path := renewalCredentialBindingPath("/config", state)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cmd := bindingProcess(t, state, config.Domain)
	cmd.Env = append(cmd.Env, "MESH_BINDING_TEST_UMASK=restrictive")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publish under restrictive umask: %v: %s", err, output)
	}
	if _, err := readSecureFile(path, privateNamesConfigMaximum); err != nil {
		t.Fatalf("published binding is not mode 0600: %v", err)
	}
}

func TestCredentialBindingFreshLockWithRestrictiveUmask(t *testing.T) {
	state := bindingTestDirectory(t)
	path := renewalCredentialBindingPath("/config", state)
	// Directory creation already obeyed umask before credential binding locks.
	// Only lock and binding publication are new behavior under test here.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := bindingProcess(t, state, "sprockt.dev")
	cmd.Env = append(cmd.Env, "MESH_BINDING_TEST_UMASK=restrictive")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh lock under restrictive umask: %v: %s", err, output)
	}
	if _, err := readSecureFile(path, privateNamesConfigMaximum); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode %04o, want 0600", info.Mode().Perm())
	}
}

func TestCredentialBindingRejectsDamagedLock(t *testing.T) {
	for _, mode := range []os.FileMode{0o000, 0o400, 0o644} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			state := bindingTestDirectory(t)
			path := renewalCredentialBindingPath("/config", state)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path+".lock", mode); err != nil {
				t.Fatal(err)
			}
			err := bindRenewalCredentials("/config", state, PrivateNamesConfig{Domain: "sprockt.dev", ZoneID: "zone"})
			if err == nil {
				t.Fatal("accepted damaged lock")
			}
			info, err := os.Stat(path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode {
				t.Fatal("modified damaged lock")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published binding despite damaged lock: %v", err)
			}
		})
	}
}

func TestCredentialBindingLockSyncFailure(t *testing.T) {
	directory := bindingTestDirectory(t)
	path := filepath.Join(directory, "binding.lock")
	injected := errors.New("injected lock inode fsync failure")
	calls := 0
	lock, err := acquireCredentialBindingLock(path, func(file *os.File) error {
		calls++
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect prepared lock: %w", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("sync before secure lock mode: %04o", info.Mode().Perm())
		}
		return injected
	})
	if lock != nil {
		t.Cleanup(func() { _ = lock.release() })
	}
	if !errors.Is(err, injected) {
		t.Fatalf("accepted unsynced lock inode: %v", err)
	}
	if calls != 1 {
		t.Fatalf("lock sync calls %d, want 1", calls)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published unsynced lock: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed lock publication left entries: %v", entries)
	}
	repaired, err := acquireCredentialBindingLock(path, (*os.File).Sync)
	if err != nil {
		t.Fatal(err)
	}
	if err := repaired.release(); err != nil {
		t.Fatal(err)
	}
}
