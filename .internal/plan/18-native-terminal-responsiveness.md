# Native terminal responsiveness: Mosh ideas over Mesh transport

Status: proposed for review, 2026-11-17

Mesh source: `2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02`

Mosh source: `decd9b705eb81626f694335b8d5940538beb06da`

## 1. Decision and intended result

Bro, build native terminal responsiveness around Mesh's existing workers,
authentication, attachment ownership, and transports: bounded screen catch-up,
conservative local typing prediction, and accurate connection/input feedback.

Lowkey, this is a terminal protocol and rendering project; success means a more
responsive interactive attachment while Mesh keeps its broader machine-control,
file, application, and service behavior.

Bro, the first implementation uses the current ordered transport and independent
Go code; an external Mosh adapter remains a useful comparison, while replacing
the transport stays a separate, measurement-driven decision.

Fr, this PR contains the research and implementation sequence only; runtime
behavior, dependencies, installed services, and existing sessions are unchanged.

### Relationship to existing work

| Existing home | Relationship of this proposal |
| --- | --- |
| [Plan 11](11-session-contract-and-boundaries.md), persistence/fidelity and interaction diagnosis | Concrete execution proposal for its terminal interaction work; reuse the corpus and failure matrix; no claim that its wider issues are resolved |
| [Plan 09](09-residual-hotspots.md#transport-comparison-baseline) | Shared comparative baseline and measurement provenance; its historic results are not evidence of current network latency |
| [T01](../tasks/T01-vt-snapshot.md) | Existing active-screen reconstruction, including explicitly documented fidelity limits |
| [T02](../tasks/T02-outbound-queue.md) | Preserve nonblocking PTY drainage, bounded queues, and ordered exit delivery |
| [ADRs 001–003](../../docs/adr/README.md) | Host-owned workers, absolute output offsets, one attacher |
| [ADR 007](../../docs/adr/007-innermost-detach-key.md) | Preserve the current nested-detach contract and existing key handling |

## 2. What the current code actually does

Bro, these are source observations at the Mesh revision above, with proposals
kept in the later sections; no WAN performance measurements were made for this PR.

| Area | Observed behavior | Evidence |
| --- | --- | --- |
| PTY output | Capture offer, output ring, shadow terminal, then live attachment queue; ring write and enqueue ordered under the worker lock | [M1][m-worker] |
| Screen model | Pinned `x/vt` behind Mesh's `Screen`; monotonic revision on accepted output and resize | [M2][m-screen] |
| Query ownership | Shadow renderer closes its reply pipe; the physical attached terminal answers live queries | [M2][m-screen] |
| Snapshot fidelity | Active cells/cursor/style/link and parser tail; no full terminal continuation checkpoint | [M2][m-screen], [M3][m-fidelity], [T01](../tasks/T01-vt-snapshot.md) |
| Fidelity tests | Some goldens intentionally record source/replay differences; `Restorable=true` can coexist with omitted completed modes, queries, and OSC 52 | [M3][m-fidelity], [M4][m-corpus] |
| Reattachment | Raw replay within the ring or an announced snapshot at an exact output boundary; candidate checks before ownership transfer | [M5][m-serve] |
| Live client | Raw frame bytes written to the real terminal; no authoritative client cell model or prediction overlay | [M6][m-attach] |
| Batching | Adjacent raw data combined, normally up to 32 KiB / 2 ms; order preserved | [M7][m-batch] |
| Slow consumers | Worker and daemon queues are bounded FIFO queues; overflow closes/disowns the attachment | [M1][m-worker], [M8][m-relay] |
| Resume accounting | Transport advances its byte checkpoint while observing a frame, before the CLI finishes writing output | [M9][m-client] |
| Input | Input frames have no sequence identity; failed sends/reconnect-era buffered input are discarded | [M10][m-frame], [M6][m-attach], [M11][m-input-reset] |
| PTY write completion | Worker checks attachment ownership, then calls `PTY.Write`; the current path discards the returned byte count | [M1][m-worker] |
| Compatibility | Four known binary kinds; unknown kinds fail decoding; daemon validates worker controls; retained workers use worker protocol 1 | [M10][m-frame], [M8][m-relay], [M12][m-release] |

Bro, the decisive correction to the earlier sketch is that existing
`Snapshot().Restorable` means a safe active-screen reconstruction under its
current contract, not permission to replace any live output interval.

## 3. Research conclusions worth carrying forward

| Mosh finding | Mesh decision |
| --- | --- |
| Screen state and user input have different synchronization semantics; the terminal display can supersede earlier states, while input retains ordered events ([paper][paper], [input state][mosh-input]) | Separate display revisions, PTY output offsets, and input identities |
| Deltas name their source state and target state; receivers retain usable baselines ([schema][mosh-schema], [receiver][mosh-receiver]) | Start with independent full display checkpoints; add deltas only against a retained acknowledged baseline |
| Prediction uses a speculative overlay and confidence epochs, with conservative invalidation ([overlay][mosh-overlay], [overlay types][mosh-overlay-types]) | Keep authoritative state separate from tentative painting; establish confidence from observed echo behavior |
| A delivery ACK can arrive before an application's echo; Mosh has a server-side echo-evaluation frontier, currently using 50 ms ([echo state][mosh-echo], [timeout definition][mosh-echo-header]) | Distinguish receipt, successful PTY writes, and timed echo evaluation; measure the timeout on Mesh workloads |
| Mosh's server is a terminal endpoint and returns query responses to the PTY ([server][mosh-server], [terminal state][mosh-echo]) | Keep the real client as query owner in the first Mesh mode; explicit barriers around every query and effect |
| Mosh chooses effect policies, including latest title/clipboard state and a bell counter ([display][mosh-display]); visible-state transport does not preserve complete native scrollback ([FAQ][mosh-home]) | Preserve current raw effects and normal-screen scrollback initially; do not inherit Mosh's tradeoffs accidentally |
| Mosh adapts sending to RTT ([sender][mosh-sender]); WebSocket is layered over TCP and TCP delivers an ordered byte stream ([RFC 6455][ws], [RFC 9293][tcp]) | Bound unsent work and outstanding states; no claim that newer bytes can bypass already-sent missing bytes on the same TCP stream |

Fr, Mosh's published latency results belong to its historical experiments;
none are predictions or measurements of Mesh.

Bro, the inspected Mosh source is GPL-3.0-or-later with additional notices in
individual files; this plan uses published ideas and original implementation,
with a separate licensing decision before copying or translating source or
tests ([source notice][mosh-license]).

## 4. Scope and invariants

| Preserved contract | Consequence for implementation |
| --- | --- |
| Host owns session; worker owns PTY | New display and prediction state never becomes a prerequisite for keeping the command alive |
| Daemon restart/upgrade preserves workers | Capability negotiated with each actual worker; raw attachment available to retained old workers |
| One attacher | State/input acknowledgments scoped to the current worker instance and attachment generation |
| Absolute PTY byte offsets | Existing `KindData.Seq` and ring semantics retained; display checkpoints carry their own coverage metadata |
| Terminal traffic direct host-to-host | No coordinator/proxy addition or public exposure |
| Existing auth and scoped controls | Terminal extensions inherit attachment authorization and direction checks |
| Exact output consumers | Logs, agent execution output, file transfers, and other controls keep their current semantics |
| Real terminal effects | Query replies, clipboard, bells, titles, and mode changes remain ordered raw effects in v1 |
| Session recovery limits | Worker death/reboot still interrupts execution; no command-execution guarantee inferred from input ACKs |

Bro, initial catch-up targets certified alternate-screen painting; normal-screen
output remains raw so shell/build scrollback keeps its current behavior.

Fr, local prediction is a separate normal-screen workstream and can ship before
catch-up if its own gates pass; it does not depend on dropping raw output or
collecting offline keystrokes.

## 5. Proposed protocol and mixed-version behavior

### 5.1 Identities and checkpoints

Bro, the following names describe the contract; binary encoding and exact Go
field names belong in the implementation PR alongside round-trip tests.

| Field | Meaning and validation |
| --- | --- |
| `worker_instance` | Identity of this running worker, distinct from a reusable short session ID |
| `attach_epoch` | New ownership/attachment generation; stale display ACKs, input IDs, and timers rejected |
| `geometry_epoch` | Resize generation; old-geometry display transactions never applied to the new size |
| `display_revision` | Monotonic state identity within the worker/geometry contract; independent of output bytes |
| `output_end` | Exact PTY boundary represented by a complete display transaction; no claim that every earlier byte reached the physical terminal |
| `base_revision` | Absent for full checkpoints; explicit, retained baseline for a future delta |
| `input_end` | Cumulative byte position in the negotiated attachment's input stream |
| `pty_written_through` | Successfully written input prefix, including partial-write accounting |
| `echo_evaluated_through` | Timed input frontier paired with an authoritative output boundary/revision; a heuristic for prediction evaluation |

Bro, generate an incarnation for every capable worker lifetime, regardless of
its creation path; a short session ID or PID is insufficient, and any reused
reservation identity needs proof that it identifies this exact lifetime.

### 5.2 Negotiation at every hop

Bro, client-offered JSON alone is insufficient because old daemons forward an
attach payload but reject unfamiliar worker replies; relay support must be
established on the actual worker connection ([M8][m-relay]).

1. Bro, the client offers optional terminal feature versions in ordinary attach
   metadata; absence selects today's raw behavior.
2. Fr, for an offered extension, a new daemon sends a worker-only
   `terminal.capabilities` preflight on the **same newly opened Unix connection**
   that will carry the attach; a new worker records the negotiated relay support
   in connection-local state.
3. Bro, the worker selects the intersection of client offer, connection-local
   relay support, and worker support; a caller-provided `relaySupported` field
   cannot establish that support.
4. Fr, a recognized legacy unsupported response closes the probe connection and
   causes one fresh raw connection under the original absolute attach deadline;
   the predecessor response is `TypeError`, matching session ID, and exactly
   `expected session.attach`, emitted before resize or ownership transfer
   ([M5][m-serve]).
5. Bro, new workers use a typed unsupported response; EOF, timeout, authentication
   failure, truncation, and unexpected messages remain failures, not fallback
   signals.
6. Fr, exactly one real attach follows negotiation; a capable worker with no
   shared native mode accepts raw in that one `session.attached` response, so
   negotiation never steals or resizes twice.

Bro, the network-facing daemon must reject worker-only preflight controls;
direct local native attachments can perform the same hop negotiation, and the
ordinary SSH adapter advertises raw support initially.

| Client / daemon / worker | Expected result |
| --- | --- |
| Old / any / any | Raw; no unfamiliar kinds or controls emitted |
| New / old / new | Raw; no connection-local relay negotiation |
| New / new / old | Recognized preflight refusal, fresh raw connection, one attach |
| New / new / new | Selected extensions only after all-hop negotiation |
| New / new / mixed retained workers | Per-worker selection, independent of daemon build |
| SSH adapter / new / new | Existing raw path |
| Mid-connection capability change | No silent switch; explicit fence or a fresh negotiated attachment |

### 5.3 Display transactions and applied acknowledgments

Bro, add a negotiated display envelope carrying the identities above, a bounded
full checkpoint, and an explicit transition/rebase marker; keep legacy
`KindSnapshot` and its attach contract unchanged.

Fr, a live display rebase must never masquerade as `session.attached`, because
that message currently changes input-generation state and invokes reconnect
input handling ([M9][m-client]).

Bro, the client validates and decodes the entire transaction, commits its model
and terminal write, and only then acknowledges the applied revision and output
boundary; local terminal write completion is not proof that pixels were scanned
out by the terminal application.

Fr, transport-received, application-applied, and speculative display state need
separate bookkeeping; `observeFrameLocked` cannot advance a native display
resume checkpoint merely because a frame arrived.

Bro, a locally observed resize immediately disarms prediction and suppresses
old-geometry paints; the client consumes their bytes without claiming they were
applied, while an ordered geometry fence retires their credit reservations
exactly once and initializes a fresh bounded window.

Fr, that fence uses reserved control capacity and cannot depend on a display
credit; it preserves ordered raw/effect barriers and requires a certified
new-geometry rebase covering superseded state before raw continuation.

Bro, reconnect discards partial transactions and speculative state, renegotiates
with the actual worker, and obtains an explicit full checkpoint or the existing
raw resume path; a missing delta baseline is always recoverable through a full
checkpoint once deltas exist.

## 6. Terminal fidelity before live catch-up

### 6.1 Explicit state/effect contract

| Terminal feature | Initial policy |
| --- | --- |
| Known alternate-screen cells, styles, cursor moves, erase operations | Candidate for coalescing inside a certified paint-only interval |
| Normal-screen printing and scrolling | Raw, preserving native scrollback behavior |
| Cursor position, pending wrap, pen, hyperlink | Exact capture/preservation required at every replacement boundary |
| Origin/insert/autowrap modes, margins, charsets | Known state required; changes are reliable barriers until explicitly supported |
| Saved cursor registers, inactive buffer, tab stops, protected cells | Preserved untouched or excluded by eligibility; no synthetic save/restore shortcuts |
| Alternate-screen enter/leave | Reliable raw transition; no repeated `1049` in live repaint |
| Device/cursor/mode/color queries | Reliable barrier; real attached terminal remains sole query responder |
| OSC 52, title/directory changes, bell, palette changes | Reliable raw effects initially; never reconstructed from a latest-screen image |
| Synchronized-output regions | No coalescing or overlay injection inside the region until separately modeled |
| Graphics, DCS/APC extensions, unknown controls | Raw fallback; unsupported parser behavior never silently discarded |
| Incomplete UTF-8 or escape sequence | No replacement/prediction injection until a known parser-ground boundary |
| Resize/reflow and soft-wrap metadata | Observationally equivalent hidden state required before the first skipped byte; a later barrier cannot repair erased wrap/reflow state |

Bro, the model needs an explicit eligibility/unknown-state result separate from
`Capture.Restorable`; default-unknown is essential after an incomplete legacy
snapshot or an unsupported terminal action.

Fr, eligibility certifies hidden-state preservation before any replacement,
including soft-wrap metadata that later resize or raw output can observe;
fallback after a destructive repaint cannot retroactively make that repaint
correct, so an uncertified interval stays raw from the outset.

### 6.2 Small first implementation

1. Bro, extend the terminal wrapper with immutable state capture and a separate
   live-paint serializer for the supported profile; retain the existing snapshot
   API for its current callers.
2. Fr, classify each PTY chunk **before** applying it to the screen for catch-up
   scheduling; a chunk containing a query, effect, unknown action, or incomplete
   sequence is entirely noncoalescible in v1.
3. Bro, before such a chunk, freeze the preceding dirty screen state and order
   its paint before the original chunk; then forward the chunk byte-for-byte and
   update model/eligibility in the normal sequence.
4. Fr, pure eligible paint chunks can update one pending target; later eligible
   chunks replace that target before serialization without skipping a barrier.
5. Bro, a mode transition/fallback carries an explicit output boundary; the next
   raw data starts at that boundary, with no invented PTY bytes or hidden gap.

Bro, a query in the middle of a PTY read must observe the state at that point;
capturing the end of the read and replaying the query afterward gives the wrong
answer, which is why the first classifier keeps the whole barrier-containing
chunk raw.

Fr, the scheduler must capture before a barrier mutates the screen; the current
`commitPTYChunk` feeds the complete chunk before enqueueing it, so this requires
an intentional refactor while preserving its ring/ownership ordering.

### 6.3 Fidelity gate

Bro, screenshot equality alone is insufficient; the same future raw bytes must
produce the same behavior after a replacement as after uninterrupted output.

| Required oracle | Coverage |
| --- | --- |
| Semantic state assertions | Active cells/styles, cursor, pen/link, relevant modes, parser-ground status, pending wrap, geometry |
| Continuation comparison | Subsequent DSR, saved-cursor restore, margin/scroll operations, alternate-screen exit, and resize |
| Independent terminal fixture | Raw reference and optimized path driven through the same pinned terminal/tmux environment; compare cells plus query responses/effects |
| Existing corpus retained | Existing observations preserved; new expected-correct assertions added alongside deliberately lossy goldens |
| Unsupported trace | Raw fallback before any irreversible skip, with byte/effect order intact |

Fr, the independent fixture is a development dependency with pinned versions;
the runtime stays on the existing Charm terminal stack behind Mesh-owned types.

## 7. Bounded scheduling across the entire path

Bro, the worker owns semantic coalescing; the daemon relays complete transactions
and never independently drops deltas or barrier prefixes.

| Resource | Proposed bound/ownership |
| --- | --- |
| Pending eligible display | One latest dirty target per attachment |
| In-flight checkpoints | Negotiated transaction and byte credits; hard maxima validated against existing frame/queue limits |
| Raw/effect barriers | Existing bounded FIFO accounting and control reserve; no silent discard |
| Delta baselines, later | Explicit bounded retained set; unavailable baseline requests a full checkpoint |
| Frame size | At most `protocol.MaxPayload`, including metadata; oversized state falls back before selecting the mode |
| Geometry | Existing axis/cell limits plus bounded capture/serialization allocations |
| Timers | Work only while dirty, unacknowledged, or awaiting echo evaluation; no idle per-session repaint loop |

Fr, begin the correctness prototype with one outstanding checkpoint and one
replaceable pending target, then measure a small bounded credit window before
shipping; one outstanding state limits the display cadence to roughly one RTT.

Bro, credits remain end-to-end until client application acknowledgment or
explicit epoch cancellation, so a
fast worker-to-daemon link cannot fill a slow daemon-to-client link with obsolete
screen states; separately retain bounded budgets for raw output and controls.

Fr, capture/serialization happens at a send opportunity or required barrier,
not once per PTY read; freeze consistent data under the existing ownership lock
and do expensive encoding outside it where the terminal API permits.

Bro, a capacity failure before safe coalescing uses the existing detach/resume
behavior; no network wait enters the PTY pump, no unbounded barrier backlog is
introduced, and final output remains ordered before exit.

Fr, process exit freezes the final eligible paint and orders it before the exit
control; credit waits and writes share the existing bounded final-flush lifetime.

Bro, if that lifetime or queue budget expires, retire the attachment without
claiming final display delivery while exit metadata, capture, and worker
finalization complete; missing ACKs never permit unbounded waiting or a bypass
of the negotiated credit limit.

Fr, the initial scheduler should stay raw on a healthy link when checkpoint
encoding would cost more than the pending raw work; thresholds and credit counts
are measured parameters, not copied Mosh constants.

Bro, queued application work can be superseded before it reaches the socket;
already-sent bytes on a TCP stream remain ordered, and opposite-direction input
does not inherently wait behind incoming output bytes ([RFC 9293][tcp]).

## 8. Conservative local prediction without offline replay

### 8.1 Client architecture

Bro, add a passive authoritative client screen mirror and a separate speculative
overlay, with a single owner for terminal writes; prediction never modifies the
authoritative model, output offsets, logs, or the bytes sent to the remote app.

Fr, on the raw path the output owner removes the overlay at a known parser-ground
boundary, processes and forwards the original raw bytes, and reapplies an
eligible overlay only after the parser is ground again.

Bro, removing an overlay restores the original covered cells, cursor, style,
and hyperlink using explicit operations; it must not use save/restore registers,
scroll, change terminal modes, or answer queries.

### 8.2 Initial eligibility and confidence

| Allowed first | Disarm/reset conditions |
| --- | --- |
| Append-only single-cell printable ASCII on a known normal-screen row | Enter, Escape, control input, completion, navigation, paste, mouse/focus reports |
| Known cursor, covered cells, pen/link, parser-ground state, and relevant modes | Resize, alternate-screen transition, fallback, takeover, reconnect, uncertain capture |
| No wrapping, scrolling, last-column painting, or wide/combining/grapheme cells touching the write-and-restoration footprint | Non-single-cell input or affected cells, unknown widths, malformed/partial UTF-8, synchronized-output region |
| Tentative predictions initially hidden; confidence after a real new echo match | Mismatch or unexpected screen movement; blank/already-present character matches do not establish confidence |
| Prediction visible only with an explicit speculative presentation | No claim of remote completion; original authoritative state always recoverable |

Bro, the first version deliberately leaves backspace/editing and full-screen
prediction for follow-up fixtures; this keeps the initial overlay reversible
and useful for ordinary append typing.

Fr, old-session eligibility may be sparse because today's attachment snapshot
omits modes; keep prediction disarmed until a certified baseline or observed
reset establishes every required condition, and measure actual eligibility
coverage before enabling it broadly.

Bro, `ECHO`/`ICANON` are not sufficient application-echo oracles: line editors
can implement echo themselves, so any optional platform probe reports known or
unknown information without substituting for confidence and correction tests.

### 8.3 Input identities and echo evaluation

1. Bro, introduce a separately negotiated tracked-input envelope with attachment
   epoch and input byte position; assign positions only to bytes actually
   submitted after local detach-key filtering.
   Fr, its send result distinguishes submitted-to-link, discarded-before-send,
   and failed/uncertain; discarded input advances neither the sequence nor
   prediction tracking, and uncertain submission retires the input epoch until
   a fresh attachment establishes the next one.
   Bro, the new envelope must use the existing reconnect input gate; the legacy
   path can return success while discarding reconnect-era input, which cannot
   authorize a prediction or a tracked sequence advance ([M9][m-client]).
2. Fr, validate ownership and contiguous sequence at the worker; track successful
   PTY write counts, including short writes and error-after-prefix, before
   advancing `pty_written_through`.
3. Bro, start echo evaluation from the successfully written prefix; publish the
   timed frontier with the screen revision and `output_end` under the same
   ordering that binds output capture to delivery.
4. Fr, send frontier-only updates when needed even if cells did not change;
   reconcile predictions only after the client has applied the corresponding
   authoritative output/state boundary.
5. Bro, start from Mosh's 50 ms value as an experiment parameter and test loaded
   apps with much later echoes; the frontier remains a heuristic and must allow
   a mistaken prediction to disappear and later authoritative output to arrive
   without corrupting the terminal.

Bro, receipt by the daemon, admission to a worker queue, successful PTY write,
and elapsed echo-evaluation time are different facts; none proves that a command
ran or finished.

Fr, takeover discards queued input that has not started writing and revokes its
ACK/prediction authority; bytes already accepted by the PTY or handed to an
in-progress write cannot be undone retroactively.

Fr, preserve today's input-discard/reconnect-flush policy initially, clear
predictions on uncertainty, and never retransmit a failed batch automatically
as a side effect of adding prediction.

## 9. Failure behavior and rollback

| Event | Required behavior |
| --- | --- |
| Connection loss | Worker/process continue; drop tentative overlay; discard incomplete display transaction; preserve existing offline-input policy |
| Reconnect | Renegotiate every actual hop; new attachment generation; raw resume or full certified checkpoint |
| Daemon restart/upgrade | Worker survives; old workers remain attachable raw; no capability inferred solely from daemon version |
| Takeover | Revoke prior attachment's ACK/input/timer authority; clear client predictions; retain single-attacher semantics |
| Resize | Geometry fence; predictions cleared; cancel old credit reservations without an applied ACK; retain authoritative state until a certified new-geometry rebase covers superseded output, and require that rebase before raw continuation; otherwise recover attachment without advancing the committed boundary |
| Unsupported terminal action | Ordered raw barrier/fallback before unsafe skipping; no loss of query/effect bytes |
| Output/write failure | No applied acknowledgment for an incomplete write; terminal cleanup and session ownership follow existing failure paths |
| Worker exit | Ordered final output/checkpoint and exit when credits recover within the existing flush lifetime; bounded attachment retirement otherwise, with normal exit metadata/capture completion |
| Worker death or reboot | Session interruption semantics retained; no reuse of old worker/input identities |
| Feature disabled | Raw attachment available without restarting worker or command; any midstream transition uses an explicit safe fence |

Bro, expose independent per-attachment display and prediction preferences,
provisionally `--terminal-display=raw|auto` and `--predict=off|auto`; default both
to existing raw/no-prediction behavior through the experimental rollout.

Fr, diagnostics should report selection/fallback reason, pending/in-flight byte
counts, applied revision age, and prediction reset/mismatch counters without
logging keys or terminal contents.

## 10. Implementation sequence and reviewable PRs

Bro, each row is a separately reviewable implementation unit with an exit gate;
phase numbers express dependency order, not an unmeasured time estimate.

| Phase | Concrete changes | Exit gate |
| --- | --- | --- |
| 0 — Baseline and contract | Extend Plan 09 receipts for interactive latency and screen freshness; finalize supported terminal profile and Plan 11 failure/effect matrix | Reproducible raw baseline, pinned terminal/Mosh/tmux versions, measured variance, explicit acceptance targets |
| 1 — Fidelity primitives | Mesh-owned immutable state/eligibility interface, live-paint serializer, parser/effect classifier, semantic continuation fixtures | Certified paint-only intervals observationally equivalent; unsupported sequences stay raw; existing lossy snapshot contract not relabeled |
| 2 — Negotiated protocol | Worker connection-local preflight, additive attach offers, native envelopes, generation checks, client-applied checkpoints, explicit mode fences | Complete client/daemon/worker compatibility matrix; auth/direction negatives; no double attach/resize/steal |
| 3 — Screen catch-up | Worker scheduler, end-to-end display credits, daemon routing, alternate-screen eligibility and ordered barriers | Bounded memory/pump progress, fresh-screen benefit under supported overload, zero fidelity/effect regressions |
| 4 — Input/echo frontiers | Tracked input while online, partial-write accounting, ordered PTY-written/echo-evaluation feedback | Correct input prefix under short writes/errors/takeover; delayed-echo tests; existing no-replay semantics |
| 5 — Prediction | Passive client mirror, single output owner, reversible ASCII overlay, confidence epochs, visible speculative state | No-echo/password-transition and correction fixtures; measurable local latency benefit and useful eligibility coverage |
| 6 — Rollout and tuning | Raw/auto and off/auto preferences, diagnostic counters, measured credit/cadence tuning, mixed-version and retained-worker proofs | Correctness gates plus agreed performance/resource gates; simple raw rollback |
| 7 — Optional follow-ups | Acknowledged-baseline deltas, broader editing/Unicode prediction, fuller terminal-state support | Separate evidence of benefit and dedicated fidelity fixtures for each expansion |

Fr, phases 3 and 4 can proceed independently after phases 1–2, and phase 5
depends on phase 4 plus its raw-path fidelity gates rather than completion of
phase 3.

### File-level change map

| Existing area | Planned work |
| --- | --- |
| `internal/terminal/screen.go` and terminal tests | Keep snapshot callers stable; add bounded immutable state/eligibility and a distinct live renderer in new focused files |
| `internal/protocol/frame.go`, `control.go`, new terminal schema file | Versioned offers/envelopes, transaction limits, direction and identity validation |
| `internal/worker/serve.go`, `worker.go`, new display scheduler file | Capability preflight, atomic attachment selection, barrier-aware pump integration, credit accounting, input/echo frontier |
| `internal/daemon/relay.go` and relay tests | Hop negotiation, known-control routing, bounded transaction forwarding, authorization inventory |
| `internal/transport/client.go`, `batch.go` and tests | Applied checkpoint integration, reconnect clearing, explicit rebase, no batching across new semantic fences |
| `internal/cli/attach.go`, `attach_input.go`, new prediction/output-owner files | Client mirror, overlay lifecycle, mode selection, truthful connection feedback, preserved detach handling |
| `internal/sshd` and existing integration fixtures | Raw compatibility assertions; no implicit prediction capability for ordinary SSH clients |
| `scripts/bench/` | Isolated network/PTY trace harness and receipts; current Unix throughput benchmark retained as a different measurement |
| Release/retained-worker proof fixtures | Actual old worker/new daemon and rollback evidence; preserve supported worker protocol range |
| `docs/adr/` and terminal guide | Accepted decisions and supported behavior when implementation is ready; generated CLI reference only after flags/help change |

Bro, no new runtime dependency is selected; reuse pinned `x/vt`, `x/ansi`,
Ultraviolet, and existing protocol/transport primitives behind Mesh-owned types,
with any necessary upstream/fork changes justified in their own task brief.

## 11. Verification and measurement plan

### Correctness matrix

| Test group | Required cases |
| --- | --- |
| Wire compatibility | Old/new permutations; absent capability; unsupported preflight; malformed/oversized envelope; stale epoch; unknown native message before negotiation |
| Transaction application | Truncation before/inside payload; terminal-write failure; ACK only after full commit; reconnect before/after commit; duplicate/stale ACK; cancelled geometry retires credits without claiming application |
| State fidelity | ANSI split at every byte boundary; UTF-8 splits; saved cursor; margins; mode changes; soft wraps; inactive buffer; alternate enter/exit; resize continuation |
| Effects/queries | DSR/DA/DECRQM and color query in the middle of a read; one responder; correct pre-query state; OSC 52 marker, bell/title order; unknown DCS/graphics fallback |
| Queue/flow control | Slow worker socket, slow daemon socket, slow physical terminal, withheld ACK, multiple sessions; resize with all credits outstanding; bounded counts and memory |
| Lifecycle | Drop client, daemon restart, takeover, detached-only refusal, host interruption; exit with unacked state plus newer pending target, covering credit recovery and timeout; resize with pending skipped source bytes and no false resume advance |
| Input | Partial PTY writes, error after a prefix, stale owner, received-but-unwritten bytes, discarded-vs-submitted send result, no sequence gap on silent discard, new kind using reconnect input gate, detach filtering, no implicit retry |
| Prediction | Delayed/no echo; password-style transition after Enter; already-present/blank match; correction; paste/control/Unicode disarm; ASCII beside/on either half of wide glyphs or combining clusters disabled with neighbors unchanged; resize/reconnect cleanup |
| Overlay interleaving | Raw queries while predicted cells visible; partial ANSI chunk followed by local typing; single serialized terminal writer; no changed save/restore state |
| Authorization | Denied attachment and wrong-direction controls; no worker-only preflight from network client; ACK/input only for authorized current owner |

Bro, extend existing `internal/terminal/{screen,corpus,parser_corpus,fidelity}_test.go`,
worker/daemon relay tests, `internal/transport/{input,lifecycle,detached_resume}_test.go`,
and `internal/cli/attach_reconnect_test.go`; preserve the distinction between
historical observation goldens and newly required semantic assertions.

Fr, integration coverage belongs alongside `reattach_snapshot.sh`,
`terminal_queries_do_not_wedge.sh`, `terminal_signals_reach_the_child.sh`,
`logs_does_not_attach.sh`, and existing client-death/takeover/exit fixtures;
new behavior gets focused scripts through the existing owned-process harness.

### Workloads and metrics

| Dimension | Required observations |
| --- | --- |
| Comparison paths | Native Mesh raw, proposed display with prediction off, prediction on, Mesh SSH, SSH + tmux, Mosh + tmux; equivalent persistence and output requirements |
| Link profiles | Low latency, injected 150/300 ms RTT, jitter, constrained bandwidth, output flood, temporary disconnect; actual network path recorded |
| Input latency | Local key-to-speculative-write, key-to-authoritative-echo, and physical-terminal observation separately; p50/p95/p99 and eligible-key coverage |
| Display freshness | Age of the applied screen; time to current screen after a burst/disconnect; queued bytes at every hop |
| Correctness | Missing/duplicate input, effect count/order, terminal-state/continuation mismatches, prediction corrections |
| Resource use | Worker/daemon/client CPU, allocations, heap/RSS, goroutines, network bytes, unchanged storage-write behavior |
| Workloads | Shell append typing, deliberate delayed/no echo, supported full-screen repaint, heavy build logs, Unicode/paste, resize, repeated mode changes |

Bro, record source/binary/harness hashes, build flags, OS, hardware, terminal,
geometry, network path, prediction settings, and cleanup receipts; the existing
`scripts/bench/run.py` measures local Unix attachment/throughput and cannot stand
in for WAN interactive results ([benchmark guide][m-bench]).

Fr, score visible-screen freshness separately from lossless output delivery and
native scrollback; Mosh plus tmux is a persistent-screen comparator, and skipped
intermediate output never counts as faster delivery of every byte.

Bro, use monotonic clocks and correlated fixture events for timing; do not
subtract unsynchronized wall clocks from different machines to claim latency.

Fr, loss/delay emulation runs in isolated fixtures; an application-level delay
proxy is useful for scheduling tests but does not reproduce TCP packet-loss
head-of-line behavior, which needs a controlled network fixture.

### Acceptance gates

| Gate | Required result |
| --- | --- |
| Fidelity | Zero mismatches in the declared supported semantic/continuation corpus; raw fallback for excluded cases |
| Lifecycle/input | No regression in process survival, ownership, exact-output consumers, effects, or existing input-discard policy |
| Bounds | Enforced byte/transaction/cell budgets at each hop; no growth with output duration or withheld ACK; PTY pump continues |
| Prediction responsiveness | Proposed target: p95 local prediction write within 20 ms for eligible input at injected 150/300 ms RTT on the reference desktop; authoritative latency reported separately |
| Catch-up benefit | Improvement beyond measured run-to-run noise under supported overload; recorded baseline/treatment distributions and CPU/network cost |
| Healthy link | No material regression under the phase-0-agreed latency/resource tolerance; no gratuitous full repaint on ordinary raw output |
| Idle/low resource | No new always-running per-session repaint timer; measured Pi/low-resource cost before default enablement |

Bro, phase 0 records numeric tolerances and the reference hardware before tuning;
the 20 ms prediction target is a proposed acceptance goal, not an observed result,
and a failed target keeps the feature experimental until reviewed.

Fr, implementation PRs run the repository's affected gates and the full required
attach/lifecycle integration suite, with platform-specific native checks and
retained-worker proofs where applicable:

```sh
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/gates.sh
./scripts/verify.sh
```

Bro, use the isolation/ownership requirements in `docs/development.md` and
Plan 09 for later measurements; real-host installation and production network
fault injection remain outside this plan PR.

## 12. Deliberate follow-ups

### Input recovery

Bro, replay of uncertain already-sent bytes and collecting brand-new offline
keystrokes are separate product policies; neither is necessary for prediction.

| Future recovery requirement | Concrete contract to implement before enabling |
| --- | --- |
| Identity | Running worker instance plus revocable owner lease; lease survives eligible reconnect, not takeover or worker replacement |
| Ordered deduplication | Bounded input history and cumulative written prefix; retries cannot duplicate a successfully written prefix |
| Partial writes | Per-prefix accounting and retained uncertain suffix; no ACK for unaccepted bytes |
| Reconnect policy | Explicit decision on uncertain resend, newly typed offline input, cancellation, expiry, and maximum pending bytes |
| Crash boundary | No reuse of acknowledgments across worker death/reboot; no exactly-once command-execution claim |
| Rollback | Legacy/current discard mode still usable without terminating the command |

### Deltas and transport

Fr, add row/cell deltas only if full checkpoints are a measured cost; validate
their exact acknowledged base, bound retained state, and recover missing bases
with a full checkpoint.

Bro, consider a datagram-capable display transport only if isolated loss tests
show ordered-stream blocking remains material after scheduling and prediction;
a single ordered QUIC stream would retain that ordering constraint, while a
datagram design would also need congestion control, authentication, bounded
reassembly, ACKs, and state recovery ([RFC 9000][quic], [RFC 9221][datagram]).

## 13. Completion of this planning PR

Bro, completion means a reviewable plan tied to current source, explicit fidelity
and compatibility contracts, phased file-level work, and reproducible acceptance
criteria; performance improvements and runtime correctness remain claims for
the subsequent implementation evidence.

Fr, this proposal leaves existing plan approvals and task completion checkboxes
untouched; accepted non-obvious design choices should become ADRs with the
implementation that establishes their behavior.

## Source references

Bro, Mesh code references below are pinned to the inspected main revision;
Mosh references are pinned to the inspected upstream revision, and the paper
and standards support the architectural reasoning rather than Mesh benchmarks.

[m-worker]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/worker/worker.go
[m-screen]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/terminal/screen.go
[m-fidelity]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/terminal/fidelity_test.go
[m-corpus]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/terminal/corpus_test.go
[m-serve]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/worker/serve.go
[m-attach]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/cli/attach.go
[m-batch]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/transport/batch.go
[m-relay]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/daemon/relay.go
[m-client]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/transport/client.go
[m-frame]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/protocol/frame.go
[m-input-reset]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/cli/attach_input.go
[m-release]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/internal/release/release.go
[m-bench]: https://github.com/ShaulLavo/mesh/blob/2d7418a1b0b7dc77109bad26d2ee0e119fdc6e02/scripts/bench/README.md
[paper]: https://mosh.org/mosh-paper.pdf
[mosh-home]: https://mosh.org/#faq
[mosh-input]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/statesync/user.cc
[mosh-schema]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/protobufs/transportinstruction.proto
[mosh-receiver]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/network/networktransport-impl.h
[mosh-sender]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/network/transportsender-impl.h
[mosh-echo]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/statesync/completeterminal.cc
[mosh-echo-header]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/statesync/completeterminal.h
[mosh-overlay]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/frontend/terminaloverlay.cc
[mosh-overlay-types]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/frontend/terminaloverlay.h
[mosh-server]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/frontend/mosh-server.cc
[mosh-display]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/terminal/terminaldisplay.cc
[mosh-license]: https://github.com/mobile-shell/mosh/blob/decd9b705eb81626f694335b8d5940538beb06da/src/frontend/mosh-client.cc
[ws]: https://www.rfc-editor.org/rfc/rfc6455.html
[tcp]: https://www.rfc-editor.org/rfc/rfc9293.html#section-2.2
[quic]: https://www.rfc-editor.org/rfc/rfc9000.html#section-2
[datagram]: https://www.rfc-editor.org/rfc/rfc9221.html#section-5
