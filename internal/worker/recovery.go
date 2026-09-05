package worker

import (
	"fmt"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

const checkpointInterval = 2 * time.Second

func (w *Worker) startRecovery() {
	if w.cfg.HostID == "" {
		return
	}
	w.recoveryState = recovery.Record{
		Version: recovery.Version, HostID: w.cfg.HostID, SessionID: w.cfg.ID,
		Shell: recoveryShell(w.cfg), ShellDirectory: w.cfg.Cwd,
		DirectorySource: recovery.DirectoryLaunch, Command: slices.Clone(w.cfg.Command),
	}
	w.inheritRecoveryLabel()
	w.loadExpectedAgent()
	w.checkpointWriter = recovery.NewWriter(w.cfg.Dir, func(err error) {
		log.Printf("worker: checkpoint session %s: %v", w.cfg.ID, err)
	})
	w.checkpointStop = make(chan struct{})
	w.checkpointDone = make(chan struct{})
	go w.checkpointLoop()
}

func (w *Worker) inheritRecoveryLabel() {
	if w.cfg.RecoveredFrom == "" {
		return
	}
	dir := filepath.Join(filepath.Dir(w.cfg.Dir), w.cfg.RecoveredFrom)
	previous, err := recovery.ReadSaved(dir, w.cfg.HostID, w.cfg.RecoveredFrom, recovery.Record{})
	if err != nil {
		return
	}
	w.recoveryState.Title = previous.Title
	w.recoveryState.Restart = previous.Restart
}

func recoveryShell(cfg Config) string {
	for _, entry := range cfg.Env {
		if value, found := strings.CutPrefix(entry, "SHELL="); found && value != "" {
			return value
		}
	}
	if len(cfg.Command) > 0 && isRecoveryShell(cfg.Command[0]) {
		return cfg.Command[0]
	}
	return "/bin/sh"
}

func isRecoveryShell(executable string) bool {
	switch filepath.Base(executable) {
	case "sh", "bash", "zsh", "fish", "dash", "ksh", "mksh", "ash":
		return true
	default:
		return false
	}
}

func (w *Worker) checkpointLoop() {
	defer close(w.checkpointDone)
	ticker := time.NewTicker(checkpointInterval)
	defer ticker.Stop()
	w.checkpoint()
	for {
		select {
		case <-ticker.C:
			w.checkpoint()
		case <-w.checkpointStop:
			w.checkpoint()
			w.checkpointWriter.Close()
			return
		}
	}
}

func (w *Worker) stopRecovery() {
	if w.checkpointWriter == nil {
		return
	}
	close(w.checkpointStop)
	<-w.checkpointDone
}

func (w *Worker) checkpoint() <-chan error {
	w.checkpointMu.Lock()
	defer w.checkpointMu.Unlock()
	directory := w.recoveryDirectoryObservation()
	w.mu.Lock()
	if directory != "" && w.recoveryState.DirectorySource != recovery.DirectoryShell {
		w.recoveryState.ShellDirectory = directory
		w.recoveryState.DirectorySource = recovery.DirectoryObserved
	}
	screen := w.screen.SaveText(recovery.MaxLines, recovery.MaxTextBytes)
	record := w.recoveryState
	record.CheckpointAt = w.currentTime().Round(0)
	record.LastOutputAt = w.lastOutputAt.Round(0)
	if screen.Title != "" {
		record.Title = screen.Title
	}
	w.mu.Unlock()
	record.Lines = screen.Render()
	return w.checkpointWriter.Submit(record)
}

func (w *Worker) recoveryDirectoryObservation() string {
	w.mu.Lock()
	if w.reaped || w.cmd == nil || w.cmd.Process == nil || w.recoveryState.DirectorySource == recovery.DirectoryShell {
		w.mu.Unlock()
		return ""
	}
	pid := w.cmd.Process.Pid
	w.mu.Unlock()
	observer := w.observeProcess
	if observer == nil {
		observer = defaultProcessObserver
	}
	// A foreground agent's directory is not the interactive shell's directory.
	observed := observer(-1, pid)
	w.mu.Lock()
	stillOurs := !w.reaped && w.cmd.Process.Pid == pid
	w.mu.Unlock()
	if !stillOurs || !filepath.IsAbs(observed.directory) || len(observed.directory) > recovery.MaxFieldBytes {
		return ""
	}
	return observed.directory
}

func (w *Worker) writeShellUpdate(conn net.Conn, request protocol.Control) {
	err := w.updateRecoveryShell(request)
	if err == nil {
		err = <-w.checkpoint()
	}
	w.writeRecoveryResponse(conn, request, protocol.TypeShellUpdated, err)
}

func (w *Worker) updateRecoveryShell(request protocol.Control) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.checkpointWriter == nil || w.finished || w.reaped || w.cmd == nil || w.cmd.Process == nil {
		return fmt.Errorf("worker: shell recovery is unavailable")
	}
	if request.SessionID != w.cfg.ID || request.ShellPID != w.cmd.Process.Pid {
		return fmt.Errorf("worker: recovery updates require the session's interactive shell")
	}
	if !isRecoveryShell(request.ShellExecutable) {
		return fmt.Errorf("worker: recovery update does not name a supported shell")
	}
	updated := w.recoveryState
	updated.Shell = request.ShellExecutable
	updated.ShellDirectory = request.ShellDirectory
	updated.DirectorySource = recovery.DirectoryShell
	updated.CheckpointAt = w.currentTime()
	if err := recovery.Validate(updated); err != nil {
		return fmt.Errorf("worker: shell update: %w", err)
	}
	w.recoveryState = updated
	return nil
}

func (w *Worker) writeRecoveryCommand(conn net.Conn, request protocol.Control) {
	err := w.updateRecoveryCommand(request)
	if err == nil {
		err = <-w.checkpoint()
	}
	w.writeRecoveryResponse(conn, request, protocol.TypeOK, err)
}

func (w *Worker) updateRecoveryCommand(request protocol.Control) error {
	if request.ClearRecoveryCommand == (request.RecoveryCommand != nil) {
		return fmt.Errorf("worker: provide a restart command or clear it")
	}
	if err := recovery.ValidateCommand(request.RecoveryCommand); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if request.SessionID != w.cfg.ID || w.checkpointWriter == nil || w.finished {
		return fmt.Errorf("worker: recovery command is unavailable")
	}
	w.recoveryState.Restart = request.RecoveryCommand
	return nil
}

func (w *Worker) writeRecoveryResponse(conn net.Conn, request protocol.Control, kind string, err error) {
	response := protocol.Control{Type: kind, RequestID: request.RequestID, SessionID: w.cfg.ID}
	if err != nil {
		response.Type = protocol.TypeError
		response.Message = err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}
