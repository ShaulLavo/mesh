# Review of the Mesh update plan

Reviewed on 2026-09-07. Scope: [T26](../tasks/T26-mesh-updates.md) and the local
CI change from `master` to `main`. The findings below record the original review;
their line numbers refer to the plan before revision. The later user-requested
revision is recorded in [Resolution status](#resolution-status).

## Verdict and intent

The plan needs revision before implementation. Its intent is an update command,
an update prompt, and one durable operation across the user's machines while
preserving sessions. The design has useful ownership boundaries, but several
promises lack the decisions needed to implement them. Calling design complete
at T26 line 3 was premature.

Two independent model reviews ran alongside the author's source inspection:

- Reviewer A: GPT-6 Astra, four findings.
- Reviewer B: GPT-5.6 Sol, three retained findings in its final review.
- Lead review: checked the findings against current source, the original
  incident, the installed host address book, and the user's requested scope.

## Act on

### R1. Keep release versions in source order

Priority: high. Raised independently by A and B.

T26 lines 334-342 require exact-SHA publication, serialization, and deduplication.
Those rules still allow a regression. A newer commit can finish CI and publish
before an older commit. When the older run finally passes, it can receive the
next patch version and become `latest`.

Under the publication lock, compare the candidate source with the last published
source. Skip an already superseded ancestor and reject divergent history until
its release policy is resolved. Test CI completion in reverse source order.
Checking only the tested SHA prevents publishing untested code; it does not
prevent publishing old code as a newer release.

### R2. Define and verify the rollback compatibility contract

Priority: high. A and B found different parts of this problem.

T26 lines 140-157 promise a compatibility check before activation. However, the
proposed release structure has no state compatibility metadata.
[Storage initialization](../../internal/storage/store.go) automatically applies
Goose migrations before the daemon can pass health verification. Restoring an
executable after a failure does not restore database compatibility.

The plan also checks only whether a new daemon can serve old workers. A candidate
daemon can [create a new worker](../../internal/daemon/lifecycle.go) during its
validation period. After rollback, the previous daemon must be able to serve
that new worker too.

Define state and worker compatibility in the release contract. Verify the exact
installed-to-target transition before activation, including hosts that skipped
releases. Test the previous binary against post-upgrade state. Either require
worker compatibility in both directions or defer new session creation and
recovery until activation commits. Test a session created during validation
followed by forced rollback.

### R3. Define when a target may activate

Priority: medium. Raised by A.

T26 lines 118-134 allow accepted target work to finish without the coordinator,
yet say a failure stops further activations. With two concurrent targets, one
can fail while another is disconnected and still staging. The second target
cannot know that it should stop.

Persist a separate authorization to activate after staging. A failure stops
issuing new authorizations. Already authorized targets may finish, and status
must say so. Define cancellation relative to that point. Test a failed target
while another staged target loses its coordinator connection.

### R4. Make update notices work on client-only installations

Priority: medium. Raised by A.

T26 lines 45-54 give background checking exclusively to the daemon, while
lines 103-110 preserve a client-only mode. Existing
[package installations](../../internal/cli/install.go) can drive remote hosts
without running a local daemon. Nothing in the plan refreshes their notice cache.

Specify an asynchronous CLI refresh or a release-availability response from an
authorized remote daemon. Neither attachment nor notification should require
installing a local hosting daemon. Test repeated remote attachment from a
client-only installation with an available update.

### R5. Separate immediate delivery from the updater project

Priority: high for this incident. Raised by the lead.

T26 lines 321-324 put publishing and real-machine rollout after all updater
milestones. That leaves the already implemented recovery feature undeployed
while a larger system is built. It repeats the operational delay that motivated
the work.

Add an independent first delivery step: validate the existing recovery build,
publish it, deploy it to the intended machines, and verify the new workers and
conversation capture. Implement the updater in parallel. Notification, automatic
release creation, and general package-manager support should not become
prerequisites for that immediate delivery.

## Consider

- **Fleet membership.** The address-book boundary is an explicit tradeoff, not
  an undisclosed implementation error. However, the actual PC address book
  currently contains only `omarchy`. It cannot establish that every intended
  machine is covered. Before advertising a whole-Mesh operation, establish the
  intended host inventory or state that the scope is locally known hosts.
  Both reviewers accepted the documented boundary; the lead considers its fit
  with the user's broader request unresolved.
- **Client setup scope.** T26 requires installing a hosting daemon before a
  client-only machine can coordinate a fleet update. Evaluate a supervised
  updater-only runner or an existing authorized coordinator before making that
  installation a product requirement. This is a design tradeoff, not a proved
  correctness failure.
- **Artifact retention.** A queued exact version can become unavailable if its
  published assets disappear. Define a visible failure result and whether pending
  operations pin cached artifacts. Reviewer B raised this in preliminary output
  but did not retain it among its final three findings. It does not justify
  promising catch-up regardless of publisher availability.

## Noted

The trust model needs clearer wording. The new signed update controls do not
make an existing same-user remote shell less powerful. An administrator policy
stored under that user's identity can still be changed by commands running as
that user. First enrollment must describe the authority actually relied upon.
Do not imply a stronger ownership boundary without changing that command path.

## Dismissed or narrowed

- **New critical privilege escalation through legacy enrollment, B.** Narrowed
  to the trust-model issue above. The legacy session interface already executes
  arbitrary same-user commands, including changes to binaries and policy files.
  First enrollment does not by itself grant a new level of execution authority.
  Local or SSH enrollment alone would not remove that existing access.
- **Mandatory publisher signatures as the only provenance model.** B raised
  this in preliminary output. A target independently fetching and verifying an
  exact release through a fixed official HTTPS origin is a coherent trust model.
  A caller-supplied checksum alone is insufficient. Signatures may improve the
  release contract, but the review did not establish them as a blocker under
  the fixed-origin model.
- **Coordinator election.** Intentionally absent. Durable state that resumes
  when its owner returns meets the stated contract. Adding distributed leader
  election would expand the task without resolving the findings above.

## Agreement and verification limits

Both models independently identified the source-order publication race. Both
identified rollback gaps, with different concrete execution paths. The activation
authorization and client-only notification findings came from A and were checked
by the lead. The delivery sequence and actual fleet inventory findings came
from the lead's incident context.

No updater exists yet, so this is a design and source review rather than a
runtime validation. Source inspection confirmed automatic database migration,
candidate-daemon worker creation, client-only installation support, and local
address-book ownership. The one-line CI branch correction has no finding;
`git diff --check` passes. No updater or deployment changes were applied.

## Resolution status

The user requested the fixes after the original review. The plan was revised
on 2026-09-07. Reviewer A then checked R1-R4 and the related scope decisions
against the revised document and reported no unresolved design blockers.
The lead checked the separate immediate-delivery track for R5.

| Finding | Revision |
| --- | --- |
| R1: release ordering | [Publication](../tasks/T26-mesh-updates.md#publication-must-advance-source-history) now checks source ancestry under the publication lock, reserves an exact version and SHA, and tests delayed older CI runs. |
| R2: rollback compatibility | [Compatibility](../tasks/T26-mesh-updates.md#compatibility-before-state-mutation) now requires machine-readable formats and exact tested transitions before mutation. The [worker gate](../tasks/T26-mesh-updates.md#gate-new-workers-until-commitment) covers every launch path until commitment. |
| R3: activation permission | [Durable rollout](../tasks/T26-mesh-updates.md#durable-rollout) separates staged work from journaled activation grants and defines unresolved delivery, cancellation, and failure behavior. |
| R4: client-only notices | [User experience](../tasks/T26-mesh-updates.md#user-experience) now includes asynchronous CLI refresh with a deadline and throttling. The local helper owns daemon-free operation status. |
| R5: delayed delivery | [Immediate delivery](../tasks/T26-mesh-updates.md#deliver-recovery-independently) now publishes and verifies the existing recovery fix independently of the updater build. |
| Fleet coverage | [Fleet membership](../tasks/T26-mesh-updates.md#fleet-membership) uses an explicit reviewed manifest, preserves offline members, and makes incomplete initial inventory visible. |
| Trust-model wording | [Authority](../tasks/T26-mesh-updates.md#authority-and-artifacts) distinguishes signed update controls from existing same-user command authority and defines independent official-manifest verification. |
| Artifact retention | The same section pins approved artifact bytes while targets remain pending and defines unavailable-metadata and unavailable-artifact outcomes. |
| Client setup scope | The plan retains explicit local daemon setup for fleet coordination, while notification and local-only updating work without it. This tradeoff is stated in the preview contract. |

These findings are addressed in the written plan. No updater code was added,
no runtime acceptance was executed, and no recovery release was published or
deployed by this revision. Link and document-structure checks validate the
documents only; the implementation milestones retain the behavioral tests.
