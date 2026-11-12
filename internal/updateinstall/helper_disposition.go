package updateinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type helperDisposition string

const (
	helperRunning helperDisposition = "running"
	helperStopped helperDisposition = "stopped"
)

func captureHelperDisposition(ctx context.Context, cfg HelperConfig, installed HelperInstallation) (helperDisposition, error) {
	pid, probeErr := cfg.Probe(ctx, installed)
	if probeErr == nil && pid > 0 {
		return helperRunning, nil
	}
	if cfg.State == nil {
		return "", errors.Join(errors.New("helper entry disposition requires a native manager state probe"), probeErr)
	}
	state, err := cfg.State(ctx)
	if err != nil {
		return "", err
	}
	if state.PID != 0 {
		return "", errors.Join(errors.New("prior helper manager process has an unverified image"), probeErr)
	}
	return helperStopped, nil
}

func stopOwnedRecoveryHelper(ctx context.Context, request HelperRecovery, prior helperRecoverySnapshot) error {
	if err := verifyRecoveryHelperOwnership(ctx, request, prior); err != nil {
		return err
	}
	if request.Helper.State == nil {
		return errors.New("stopped helper restoration requires a native manager state probe")
	}
	state, err := request.Helper.State(ctx)
	if err != nil || state.PID == 0 {
		return err
	}
	pid, probeErr := request.Probe(ctx, prior.installation)
	if probeErr != nil || pid != state.PID {
		candidate := HelperInstallation{Executable: filepath.Join(transactionDir(request.Helper.StateDir), "helper", request.Digest, "mesh"), Digest: request.Digest}
		pid, probeErr = request.Probe(ctx, candidate)
	}
	if probeErr != nil || pid != state.PID {
		return errors.Join(errors.New("refusing to stop an unrelated helper process during restoration"), probeErr)
	}
	if err := stopInitialAdoptionHelper(ctx, request.Helper); err != nil {
		return err
	}
	return verifyStoppedRecoveryHelper(ctx, request.Helper)
}

func verifyRecoveryHelperOwnership(ctx context.Context, request HelperRecovery, prior helperRecoverySnapshot) error {
	service, err := readMetadata(ctx, request.Helper.ServiceDir, filepath.Base(prior.installation.ServicePath), false)
	if err != nil {
		return err
	}
	if !bytes.Equal(service, prior.service) {
		return errors.New("helper service changed outside promotion; refusing restoration")
	}
	var installed HelperInstallation
	if err := readHelperInstallation(ctx, request.Helper.StateDir, &installed); err != nil {
		return err
	}
	candidate := HelperInstallation{Executable: filepath.Join(transactionDir(request.Helper.StateDir), "helper", request.Digest, "mesh"), ServicePath: prior.installation.ServicePath, Digest: request.Digest}
	if installed != prior.installation && installed != candidate {
		return errors.New("helper receipt changed outside promotion; refusing restoration")
	}
	link, err := os.Readlink(filepath.Join(transactionDir(request.Helper.StateDir), "helper", "current"))
	if err != nil {
		return fmt.Errorf("inspect owned helper launcher: %w", err)
	}
	if link != prior.installation.Executable && link != candidate.Executable {
		return errors.New("helper launcher changed outside promotion; refusing restoration")
	}
	return nil
}

func verifyStoppedRecoveryHelper(ctx context.Context, cfg HelperConfig) error {
	state, err := cfg.State(ctx)
	if err != nil {
		return err
	}
	if state.PID != 0 {
		return errors.New("restored helper was not stopped")
	}
	return nil
}

func finishStoppedHelperRestoration(ctx context.Context, request HelperRecovery, prior helperRecoverySnapshot) error {
	if err := verifyStoppedRecoveryHelper(ctx, request.Helper); err != nil {
		return err
	}
	path := helperActivationRecord(request.Helper.StateDir)
	data, err := readMetadata(ctx, request.Helper.StateDir, filepath.Join("update", "helper", "activation-pending"), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pending := strings.TrimSpace(string(data))
	if pending != request.Digest && pending != prior.installation.Digest {
		return errors.New("helper activation request changed during stopped restoration")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("clear owned stopped-helper activation: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}
