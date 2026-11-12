package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

const (
	localTargetCancel = "cancel"
	localTargetRetry  = "retry"
)

func (a *application) runLocalHostUpdate(ctx context.Context, options updateOptions, output updateOutput) (bool, error) {
	if !options.local || options.coordinator != "" {
		return false, nil
	}
	environment, err := existingAutomaticEnvironment(ctx, "")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	info, err := a.readLocalTargetHost(ctx, environment)
	if err != nil && localDaemonAbsent(ctx, environment.stateDir) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if info.HostUpdate == nil {
		return false, nil
	}
	if info.Build == nil || info.HostUpdate.Installation != updateinstall.InstallationManaged {
		return true, errors.New("this machine needs a verified Mesh-managed daemon installation; run mesh daemon installation-check")
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	return true, a.reviewAndSubmitLocalTarget(ctx, environment, info, options, output)
}

func (a *application) readLocalTargetHost(ctx context.Context, environment updateEnvironment) (protocol.HostInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dial := a.dependencies.DialPassive
	if dial == nil {
		dial = dialAgentHost
	}
	host := HostRecord{ID: environment.local.ID, MeshIdentity: environment.local.ID, Endpoint: environment.local.Endpoint, local: true}
	conn, info, err := openAgentHostInfo(ctx, host, dial)
	if err != nil {
		return info, fmt.Errorf("read local update installation: %w", err)
	}
	_ = conn.Close()
	return info, nil
}

type localTargetReview struct {
	input update.HostTargetSubmission
	plan  update.HostTargetPlan
	prior update.HostTargetStatus
}

func readLocalTargetReview(ctx context.Context, environment updateEnvironment, info protocol.HostInfo, version string) (localTargetReview, error) {
	var policy update.HostPolicy
	if err := environment.client.Call(ctx, environment.local, "host-policy.show", nil, &policy); err != nil {
		return localTargetReview{}, fmt.Errorf("read local target policy: %w", err)
	}
	var review localTargetReview
	if err := environment.client.Call(ctx, environment.local, update.HostTargetStatusAction, nil, &review.prior); err != nil {
		return review, fmt.Errorf("read approved local target: %w", err)
	}
	review.input = update.HostTargetSubmission{Version: version, CurrentDigest: info.Build.Digest, PolicyRevision: policy.Revision, ExpectedRevision: review.prior.Revision}
	if err := environment.client.Call(ctx, environment.local, update.HostTargetPlanAction, review.input, &review.plan); err != nil {
		return review, fmt.Errorf("verify local update route: %w", err)
	}
	plan, input := review.plan, review.input
	if plan.CurrentDigest != input.CurrentDigest || plan.PolicyRevision != input.PolicyRevision || plan.Next == nil && plan.Target.Binary != input.CurrentDigest {
		return review, errors.New("the local update review does not match this installation")
	}
	return review, nil
}

func (a *application) reviewAndSubmitLocalTarget(ctx context.Context, environment updateEnvironment, info protocol.HostInfo, options updateOptions, output updateOutput) error {
	review, err := readLocalTargetReview(ctx, environment, info, options.version)
	if err != nil {
		return localTargetFailure(output, options, "Mesh could not verify this update. Run mesh update --local --check --details to inspect the reason.", err)
	}
	plan, input := review.plan, review.input
	if options.check {
		return printLocalTargetPlan(output.out, info.Build.Version, plan, options, output)
	}
	if localTargetInProgress(review.prior.Phase) {
		if review.prior.Target.Metadata != plan.Target.Metadata || review.prior.PolicyRevision != plan.PolicyRevision {
			return localTargetFailure(output, options, "A different release or policy is already recorded. Inspect mesh update status --local before cancelling or retrying it; work already granted may finish.", errors.New("active local target differs from the reviewed release or policy"))
		}
		return printLocalTargetResult(output, review.prior, options)
	}
	if plan.Target.Binary == input.CurrentDigest {
		return printLocalTargetPlan(output.out, info.Build.Version, plan, options, output)
	}
	if !options.json {
		if err := printLocalTargetPlan(output.diagnostic, info.Build.Version, plan, options, output); err != nil {
			return err
		}
	}
	if !options.yes && !a.confirmLocalTarget(plan, output.diagnostic) {
		return errors.New("update cancelled")
	}
	input.Version = plan.Target.Version
	var status update.HostTargetStatus
	if err := environment.client.Call(ctx, environment.local, update.HostTargetSubmitAction, input, &status); err != nil {
		return localTargetFailure(output, options, "Mesh could not confirm that the update was saved. Run mesh update status --local before retrying.", err)
	}
	if status.Request == "" || status.Generation == 0 || status.Target.Version != input.Version {
		return errors.New("the daemon did not report the exact durable update request; inspect mesh update status --local")
	}
	return printLocalTargetResult(output, status, options)
}

type localTargetCommandError struct {
	message string
	cause   error
}

func (e localTargetCommandError) Error() string { return e.message }
func (e localTargetCommandError) Unwrap() error { return e.cause }

func localTargetFailure(output updateOutput, options updateOptions, message string, cause error) error {
	detail := struct {
		Problem string `json:"problem"`
	}{cause.Error()}
	if options.json {
		if err := writeAutomaticJSON(output.out, detail); err != nil {
			return err
		}
		return updateExit(1)
	}
	if options.details {
		if err := printUpdateDetails(output.diagnostic, detail, output.privacy); err != nil {
			return err
		}
	}
	return localTargetCommandError{message: message, cause: cause}
}

func localTargetInProgress(phase update.HostTargetPhase) bool {
	return phase == update.HostTargetReserved || phase == update.HostTargetInstalling || phase == update.HostTargetWaiting
}

func (a *application) confirmLocalTarget(plan update.HostTargetPlan, out io.Writer) bool {
	_, _ = fmt.Fprintf(out, "Update Mesh on this machine to %s? [y/N] ", SafeTerminalText(plan.Target.Version))
	answer, err := bufio.NewReader(io.LimitReader(a.dependencies.Stdin, 128)).ReadString('\n')
	return err == nil && strings.EqualFold(strings.TrimSpace(answer), "y")
}

func printLocalTargetPlan(out io.Writer, source string, plan update.HostTargetPlan, options updateOptions, output updateOutput) error {
	if options.json {
		return writeAutomaticJSON(out, plan)
	}
	if options.details {
		return printUpdateDetails(out, plan, output.privacy)
	}
	if plan.Next == nil {
		_, err := fmt.Fprintf(out, "This managed installation already runs the verified release %s.\n", SafeTerminalText(plan.Target.Version))
		return hostPolicyPrintError(err)
	}
	_, err := fmt.Fprintf(out, "Selected release: Mesh %s.\nNext verified step: %s → %s.\nMesh checks each further step before installing it.\n", SafeTerminalText(plan.Target.Version), SafeTerminalText(source), SafeTerminalText(plan.Next.Version))
	return hostPolicyPrintError(err)
}

func printLocalTargetResult(output updateOutput, status update.HostTargetStatus, options updateOptions) error {
	if err := printLocalTargetStatus(output.out, status, options.json, options.details, output); err != nil {
		return err
	}
	return updateExit(localTargetExit(status.Phase))
}

func localTargetExit(phase update.HostTargetPhase) int {
	switch phase {
	case update.HostTargetIdle, update.HostTargetCommitted:
		return 0
	case update.HostTargetReserved, update.HostTargetInstalling, update.HostTargetWaiting:
		return 2
	case update.HostTargetAttention, update.HostTargetCancelled:
		return 1
	default:
		return 1
	}
}

func printLocalTargetStatus(out io.Writer, status update.HostTargetStatus, structured, details bool, output updateOutput) error {
	if structured {
		return writeAutomaticJSON(out, status)
	}
	if details {
		return printUpdateDetails(out, status, output.privacy)
	}
	if status.Phase == update.HostTargetIdle {
		_, err := fmt.Fprintln(out, "No local update has been selected.")
		return hostPolicyPrintError(err)
	}
	text := "Update saved for Mesh " + SafeTerminalText(status.Target.Version) + "."
	if status.Phase == update.HostTargetCommitted {
		text = "Mesh " + SafeTerminalText(status.Target.Version) + " finished updating."
	}
	if status.Phase == update.HostTargetCancelled {
		text = "Local update cancelled. Work already granted may finish, including recovery."
	}
	if status.Phase == update.HostTargetAttention {
		text = "The local update needs attention. Use mesh update status --local --details to inspect it."
	}
	if localTargetInProgress(status.Phase) {
		text += " Run mesh update status --local to read progress or mesh update cancel --local to stop future steps."
	}
	_, err := fmt.Fprintln(out, text)
	return hostPolicyPrintError(err)
}

func (a *application) localTargetOperation(cmd *cobra.Command, action string, options *updateOptions) error {
	if options.coordinator != "" {
		return errors.New("local target operations require this machine; omit --coordinator")
	}
	if cmd.Flags().Changed("expected-policy-revision") || cmd.Flags().Changed("command-id") {
		return errors.New("use --local without --expected-policy-revision or --command-id")
	}
	environment, err := existingAutomaticEnvironment(cmd.Context(), "")
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	var status update.HostTargetStatus
	if err := environment.client.Call(cmd.Context(), environment.local, update.HostTargetStatusAction, nil, &status); err != nil {
		return fmt.Errorf("read local target: %w", err)
	}
	if action == localTargetCancel {
		err = environment.client.Call(cmd.Context(), environment.local, update.HostTargetCancelAction, status.Revision, &status)
	}
	if action == localTargetRetry {
		status, err = a.retryLocalTarget(cmd.Context(), environment, status)
	}
	if err != nil {
		return fmt.Errorf("%s local target: %w", action, err)
	}
	return printLocalTargetResult(updateOutput{cmd.OutOrStdout(), cmd.ErrOrStderr(), a.privacy, options.details}, status, *options)
}

func (a *application) retryLocalTarget(ctx context.Context, environment updateEnvironment, prior update.HostTargetStatus) (update.HostTargetStatus, error) {
	info, err := a.readLocalTargetHost(ctx, environment)
	if err != nil {
		return prior, err
	}
	if info.Build == nil || prior.Target.Version == "" {
		return prior, errors.New("the running image or saved target is unknown")
	}
	var policy update.HostPolicy
	if err := environment.client.Call(ctx, environment.local, "host-policy.show", nil, &policy); err != nil {
		return prior, fmt.Errorf("read retry policy: %w", err)
	}
	input := update.HostTargetSubmission{Version: prior.Target.Version, CurrentDigest: info.Build.Digest, PolicyRevision: policy.Revision, ExpectedRevision: prior.Revision}
	if prior.Next != nil && prior.Next.Binary != input.CurrentDigest {
		input.RetryEdge = &update.Edge{From: input.CurrentDigest, To: prior.Next.Binary, Platform: info.Build.Platform}
	}
	var status update.HostTargetStatus
	err = environment.client.Call(ctx, environment.local, update.HostTargetSubmitAction, input, &status)
	if err != nil {
		return prior, fmt.Errorf("submit local retry: %w", err)
	}
	return status, nil
}
