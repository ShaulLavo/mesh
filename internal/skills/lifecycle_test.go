package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedInstallBarrierKeepsPendingState(t *testing.T) {
	f := setup(t)
	f.manager.syncDir = failOnce("mesh")
	roots, err := f.manager.Apply(t.Context(), "install", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("fault did not fire")
	}
	status, err := f.manager.Status(f.root)
	must(t, err)
	t.Logf("install result=%s problems=%v; next status=%s registered=%v", roots[0].State, roots[0].Problems, status[0].State, status[0].Registered)
	if status[0].State != stateInterrupted || status[0].Registered {
		t.Error("unconfirmed installation is not pending and unregistered")
	}
	f.manager.syncDir = nil
	retried := apply(t, f, "install")
	if retried.State != stateCurrent || !retried.Registered {
		t.Fatal("explicit retry did not settle the installation and registry")
	}
}

func TestManagedRootExcludesOtherRegistries(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	second := f
	second.manager.ConfigDir = filepath.Join(t.TempDir(), "second-config")
	entered := false
	f.manager.syncDir = func(dir *os.File, label string) error {
		if label != manifestName || entered {
			return dir.Sync()
		}
		entered = true
		roots, err := second.manager.Apply(t.Context(), actionUninstall, f.root)
		must(t, err)
		t.Logf("second configuration mutation during first update: state=%s problems=%v", roots[0].State, roots[0].Problems)
		if len(roots[0].Problems) == 0 {
			t.Error("foreign registry was not excluded")
		}
		return dir.Sync()
	}
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) != 0 || roots[0].State != stateCurrent {
		t.Fatalf("owning updater failed after excluding foreign registry: %#v", roots[0])
	}
}

func TestManagedRecoveryRetainsPreviousPendingBaseline(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	f = changed(f)
	f.manager.syncDir = failOnce("SKILL.md")
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("first update fault did not fire")
	}
	if string(read(t, filepath.Join(f.root, "mesh", "SKILL.md"))) != "new skill\n" {
		t.Fatal("first publication absent")
	}
	f.manager.files = []File{{"SKILL.md", []byte("third skill\n")}, {"references/safety.md", []byte("third safety\n")}, {"references/commands.md", []byte("third commands\n")}}
	f.manager.syncDir = failOnce(manifestName)
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("second update fault did not fire")
	}
	f.manager.syncDir = nil
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	t.Logf("no owner edits, recovery state=%s modified=%v proposed=%v problems=%v", roots[0].State, roots[0].ModifiedFiles, roots[0].ProposedFiles, roots[0].Problems)
	if len(roots[0].ModifiedFiles) > 0 {
		t.Error("previous pending publication mistaken for owner edit")
	}
}

func TestManagedRecoveryRetainsPendingProposalOwnership(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	write(t, filepath.Join(f.root, "mesh", "SKILL.md"), []byte("customized"))
	f = changed(f)
	failed := false
	f.manager.syncDir = func(dir *os.File, label string) error {
		if label == "references/safety.md" && !failed {
			failed = true
			return errors.New("interrupt after proposal")
		}
		return dir.Sync()
	}
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].ProposedFiles) != 1 {
		t.Fatal("proposal not created")
	}
	proposal := roots[0].ProposedFiles[0]
	f.manager.files = []File{{"references/safety.md", []byte("third safety")}}
	f.manager.syncDir = nil
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if roots[0].State != stateModified {
		t.Fatal("the replacement bundle did not retain the customized managed file")
	}
	roots, err = f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	_, err = os.Lstat(proposal)
	t.Logf("uninstall state=%s retained=%v proposal exists=%v", roots[0].State, roots[0].RetainedFiles, err == nil)
	if err == nil {
		t.Error("unchanged pending product proposal orphaned outside registry and manifest")
	}
}

func TestManagedSameBundleRecoveryTracksEditedProposal(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	write(t, filepath.Join(f.root, "mesh", "SKILL.md"), []byte("customized"))
	f = changed(f)
	f.manager.syncDir = failOnce("references/safety.md")
	roots, err := f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].ProposedFiles) != 1 {
		t.Fatal("proposal not created before interruption")
	}
	proposal := roots[0].ProposedFiles[0]
	write(t, proposal, []byte("owner-edited proposal"))
	write(t, filepath.Join(f.root, "mesh", "SKILL.md"), []byte("new skill\n"))
	f.manager.syncDir = nil
	roots, err = f.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	t.Logf("same-bundle recovery: state=%s problems=%v", roots[0].State, roots[0].Problems)
	roots, err = f.manager.Apply(t.Context(), actionUninstall, f.root)
	must(t, err)
	_, err = os.Lstat(proposal)
	t.Logf("uninstall: state=%s retained=%v edited proposal exists=%v", roots[0].State, roots[0].RetainedFiles, err == nil)
	if err == nil && len(roots[0].RetainedFiles) == 0 {
		t.Error("uninstall reports no retained files while edited proposal survives untracked")
	}
}

func TestManagedRootExcludesDifferentStateDirectory(t *testing.T) {
	f := setup(t)
	apply(t, f, "install")
	different := changed(f)
	different.manager.StateDir = filepath.Join(t.TempDir(), "different-state")
	roots, err := different.manager.Apply(t.Context(), "update", f.root)
	must(t, err)
	if len(roots[0].Problems) == 0 {
		t.Fatal("another state directory changed the managed root")
	}
	if string(read(t, filepath.Join(f.root, "mesh", "SKILL.md"))) != "old skill\n" {
		t.Fatal("foreign manager changed the managed tree")
	}
}
