package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/updatenotice"
)

type UpdateNotice struct {
	Version string
	Pending string
}

type UpdateNoticeDismissal struct {
	Version string
	Skip    bool
}

// UpdateNoticeCallbacks are supplied only to interactive pickers. Cached is
// local-only; Refresh and Dismiss run as asynchronous picker commands.
type UpdateNoticeCallbacks struct {
	Cached  func() UpdateNotice
	Refresh func(context.Context) UpdateNotice
	Dismiss func(context.Context, UpdateNoticeDismissal) error
}

func NewUpdateNoticeCallbacks(store *updatenotice.Store) UpdateNoticeCallbacks {
	if store == nil {
		return UpdateNoticeCallbacks{}
	}
	return UpdateNoticeCallbacks{
		Cached: func() UpdateNotice {
			notice, _ := store.Cached()
			return UpdateNotice{Version: notice.Version}
		},
		Refresh: func(ctx context.Context) UpdateNotice {
			notice, _ := store.Refresh(ctx, false)
			return UpdateNotice{Version: notice.Version}
		},
		Dismiss: func(ctx context.Context, request UpdateNoticeDismissal) error {
			return store.Dismiss(ctx, request.Version, request.Skip)
		},
	}
}

func localUpdateNoticeStore(stateDir string) *updatenotice.Store {
	return updatenotice.New(updatenotice.Config{
		Directory: filepath.Join(stateDir, "update-notice"), Current: release.Current(),
	})
}

func (a *application) interactiveUpdateNotices() bool {
	for _, stream := range []*os.File{a.dependencies.Stdin, a.dependencies.Stdout, a.dependencies.Stderr} {
		if stream == nil || !term.IsTerminal(stream.Fd()) {
			return false
		}
	}
	_, err := release.CompareVersions(release.Current().Version, "v0.0.0")
	return err == nil
}

func (a *application) pickerUpdateNotice() UpdateNoticeCallbacks {
	if !a.interactiveUpdateNotices() {
		return UpdateNoticeCallbacks{}
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return UpdateNoticeCallbacks{}
	}
	a.updateNoticeScheduled = true
	callbacks := NewUpdateNoticeCallbacks(localUpdateNoticeStore(stateDir))
	cached, refresh := callbacks.Cached, callbacks.Refresh
	callbacks.Cached = func() UpdateNotice {
		notice := cached()
		notice.Pending = pendingUpdateNotice(stateDir)
		return notice
	}
	callbacks.Refresh = func(ctx context.Context) UpdateNotice {
		notice := refresh(ctx)
		notice.Pending = pendingUpdateNotice(stateDir)
		return notice
	}
	return callbacks
}

func pendingUpdateNotice(stateDir string) string {
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "runs")); err == nil {
		if text := pendingFleetUpdateNotice(stateDir); text != "" {
			return text
		}
	}
	status, err := updateinstall.Read(stateDir)
	if err == nil && status.Settings.ClientOnly && !installationFinished(status.Phase) {
		return "Local update unfinished. Run mesh update status."
	}
	return ""
}

func pendingFleetUpdateNotice(stateDir string) string {
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return ""
	}
	runs, err := store.List()
	if err != nil {
		return ""
	}
	pending := 0
	for _, run := range runs {
		if !run.Done() {
			pending++
		}
	}
	if pending == 0 {
		return ""
	}
	return fmt.Sprintf("%d unfinished update operations. Run mesh update status.", pending)
}

func (a *application) noticeBeforeAttachment(command *cobra.Command) {
	if a.updateNoticeScheduled || !a.interactiveUpdateNotices() {
		return
	}
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return
	}
	a.updateNoticeScheduled = true
	stateDir, err := paths.StateDir()
	if err != nil {
		return
	}
	store := localUpdateNoticeStore(stateDir)
	if notice, err := store.Cached(); err == nil && notice.Version != "" {
		_, _ = fmt.Fprintf(command.ErrOrStderr(), "Mesh %s is available. Run mesh update to review.\n", SafeTerminalText(notice.Version))
	}
	if due, err := store.Due(); err == nil && due {
		scheduleUpdateNoticeCheck(stateDir)
	}
}

func scheduleUpdateNoticeCheck(stateDir string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}
	command := exec.Command(executable, "update-notice-check", "--state-dir", stateDir) //nolint:gosec // invoke this Mesh executable with fixed helper arguments, without a shell
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return
	}
	go func() { _ = command.Wait() }()
}

func newUpdateNoticeCheckCommand() *cobra.Command {
	var stateDir string
	command := &cobra.Command{
		Use: "update-notice-check", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if stateDir == "" || !filepath.IsAbs(stateDir) {
				return errors.New("update notice check requires an absolute --state-dir")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			_, err := localUpdateNoticeStore(stateDir).Refresh(ctx, false)
			return err
		},
	}
	command.Flags().StringVar(&stateDir, "state-dir", "", "Mesh state directory")
	return command
}

func startUpdateNoticeChecks(ctx context.Context, stateDir string) func() {
	if _, err := release.CompareVersions(release.Current().Version, "v0.0.0"); err != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		localUpdateNoticeStore(stateDir).Run(ctx)
	}()
	return func() { cancel(); <-done }
}
