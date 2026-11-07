package access_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/update"
)

func roleObserver(state string) access.ObserveOtherRoles {
	return func(ctx context.Context, subject access.Subject) (access.OtherRoles, error) {
		if err := ctx.Err(); err != nil {
			return access.OtherRoles{}, fmt.Errorf("fixture role observation: %w", err)
		}
		full, err := identity.DeviceMembership(state, string(subject))
		if err != nil {
			return access.OtherRoles{}, fmt.Errorf("fixture role observation: %w", err)
		}
		administrator, err := update.AdministratorMembership(state, string(subject))
		if err != nil {
			return access.OtherRoles{}, fmt.Errorf("fixture updater role: %w", err)
		}
		return access.OtherRoles{FullDevice: full, UpdateAdministrator: administrator}, nil
	}
}
func fixture(t *testing.T) (string, string, string, *access.Store) {
	t.Helper()
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	subject, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := access.Open(state, host.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return state, host.ID, subject.ID, store
}
func TestManagedClassesNeverPublishLatentApproval(t *testing.T) {
	for _, role := range []string{"full", "updater"} {
		t.Run(role, func(t *testing.T) { testClassOrders(t, role) })
	}
}
func approve(ctx context.Context, state, subject, role string) error {
	if role == "full" {
		if err := identity.ApproveDevice(ctx, state, subject); err != nil {
			return fmt.Errorf("fixture approve full: %w", err)
		}
		return nil
	}
	if err := update.Trust(ctx, state, subject, false); err != nil {
		return fmt.Errorf("fixture approve updater: %w", err)
	}
	return nil
}
func testClassOrders(t *testing.T, role string) {
	t.Helper()
	state, _, subject, store := fixture(t)
	input := access.GrantInput{Subject: access.Subject(subject), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	change, err := store.Grant(context.Background(), 0, input, roleObserver(state))
	if err != nil {
		t.Fatal(err)
	}
	if err := approve(t.Context(), state, subject, role); err == nil {
		t.Fatal("scoped key obtained another class")
	}
	if _, err := store.Revoke(context.Background(), change.Revision, input.Subject); err != nil {
		t.Fatal(err)
	}
	roles, err := roleObserver(state)(context.Background(), input.Subject)
	if err != nil || roles.FullDevice || roles.UpdateAdministrator {
		t.Fatalf("latent approval after revoke: %+v %v", roles, err)
	}
	if err := approve(t.Context(), state, subject, role); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Grant(context.Background(), 2, input, roleObserver(state)); err == nil {
		t.Fatal("scope accepted an existing native role")
	}
	registered, err := store.Membership(input.Subject)
	if err != nil || registered {
		t.Fatalf("denied scope became registered: %v %v", registered, err)
	}
}
func TestManagedClassWritersSeparateProcesses(t *testing.T) {
	for _, role := range []string{"full", "updater"} {
		t.Run(role, func(t *testing.T) { separateWriters(t, role) })
	}
}
func separateWriters(t *testing.T, role string) {
	t.Helper()
	state, target, subject, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := writerProcess(ctx, t, state, target, subject, "scope")
	second := writerProcess(ctx, t, state, target, subject, role)
	if _, err := io.WriteString(first.input, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(second.input, "x"); err != nil {
		t.Fatal(err)
	}
	firstErr, secondErr := first.command.Wait(), second.command.Wait()
	first.done, second.done = true, true
	if firstErr == nil && secondErr == nil {
		t.Fatal("both class writers acknowledged success")
	}
	if firstErr != nil && secondErr != nil {
		t.Fatalf("neither writer succeeded: %v %v", firstErr, secondErr)
	}
	registered, err := access.Registered(state, access.Subject(subject))
	if err != nil {
		t.Fatal(err)
	}
	roles, err := roleObserver(state)(ctx, access.Subject(subject))
	if err != nil {
		t.Fatal(err)
	}
	if registered && (roles.FullDevice || roles.UpdateAdministrator) {
		t.Fatal("published conflicting classes")
	}
}

type childWriter struct {
	command *exec.Cmd
	input   io.WriteCloser
	done    bool
}

func writerProcess(ctx context.Context, t *testing.T, state, target, subject, role string) *childWriter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: executable, Args: []string{executable, "-test.run=^TestClassWriterProcess$"}}
	command.Env = []string{"MESH_CLASS_HELPER=" + role, "MESH_CLASS_STATE=" + state, "MESH_CLASS_TARGET=" + target, "MESH_CLASS_SUBJECT=" + subject, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
	command.Stderr = io.Discard
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &childWriter{command: command, input: input}
	t.Cleanup(func() {
		_ = input.Close()
		if !child.done && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = command.Process.Kill() })
	t.Cleanup(func() { stop() })
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("writer readiness: %q %v", line, err)
	}
	return child
}
func TestClassWriterProcess(t *testing.T) {
	role := os.Getenv("MESH_CLASS_HELPER")
	if role == "" {
		return
	}
	fmt.Println("ready")
	var start [1]byte
	if _, err := io.ReadFull(os.Stdin, start[:]); err != nil {
		t.Fatal(err)
	}
	state, target, subject := os.Getenv("MESH_CLASS_STATE"), os.Getenv("MESH_CLASS_TARGET"), os.Getenv("MESH_CLASS_SUBJECT")
	if role != "scope" {
		if err := approve(t.Context(), state, subject, role); err != nil {
			t.Fatal(err)
		}
		return
	}
	store, err := access.Open(state, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Grant(context.Background(), 0, access.GrantInput{Subject: access.Subject(subject), ExpiresAt: time.Now().Add(time.Hour)}, roleObserver(state))
	if err != nil {
		t.Fatal(err)
	}
}
func TestDamagedOtherPolicyIsNotRoleAbsence(t *testing.T) {
	for _, name := range []string{"authorized_keys", "updates/administrators.json"} {
		t.Run(name, func(t *testing.T) {
			state, _, subject, store := fixture(t)
			path := filepath.Join(state, name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Grant(context.Background(), 0, access.GrantInput{Subject: access.Subject(subject), ExpiresAt: time.Now().Add(time.Hour)}, roleObserver(state))
			if err == nil {
				t.Fatal("damaged native policy treated as absent")
			}
			if _, err := os.Stat(filepath.Join(state, "access", "scopes.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("denied scope published policy: %v", err)
			}
		})
	}
}
