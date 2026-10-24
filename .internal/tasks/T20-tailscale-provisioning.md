# T20 — Tailscale provisioning in `mesh add`

**Status:** complete · **Blocked by:** T08 · **Owns:**
`internal/bootstrap/provision.go`, `internal/bootstrap/errors.go`,
`scripts/install/`

## Goal

```bash
mesh add shaul@pi
# pi has no Tailscale. Install it with pacman? [y/N]
mesh add shaul@pi --yes --tailscale-auth-key-file ./key   # unattended
```

Before T20, adoption stopped at a diagnosis and handed the work back. `mesh add`
already knew that Tailscale was missing, knew the platform, and held an SSH
connection with root-capable access. T20 completes that adoption path.

## Implementation

`run.go` probes Tailscale immediately after platform detection. It calls
`provisionRemote` before binary selection, identity creation, or the Mesh
installer. A provisioning failure therefore cannot leave a Mesh service on the
remote host.

`discovery.go` returns a typed backend state and records whether macOS uses the
headless daemon or `Tailscale.app`. Its shell probe emits explicit variant,
executable-path, and missing markers, so unrelated "not found" text cannot be
misclassified as a missing CLI. It checks the application bundle first. A
bundle with a missing or non-executable CLI blocks headless installation rather
than allowing a conflicting daemon. The probe also checks the standard Intel
and Apple Silicon Homebrew paths because non-interactive SSH can have a minimal
`PATH`. A `Starting` state polls for up to 20 seconds. `NeedsMachineAuth` maps
to `DiagnosticTailscaleMachineAuth` and points to the admin console.

`provision.go` uses these installation paths:

- Arch and related systems use `pacman`, selected from `/etc/os-release` before
  executable discovery.
- Debian and Ubuntu systems add the official Tailscale apt repository.
- macOS uses Homebrew when `Tailscale.app` is absent.
- Other supported Linux systems use the upstream installer from commit
  `11a6255b22a3071bb63992ee8f7fbedd6d50f4d1`. Mesh checks its pinned SHA-256
  digest before sending it to the remote shell.

The provisioner models installation as ordered typed steps. The confirmation
and executor render the same steps, so the listed mutation commands are the
commands that run. It also lists the pinned checksum and sudo authentication as
preconditions when applicable. `--yes` approves that confirmation. Without
`--yes`, a non-terminal caller fails before any remote mutation.

Non-root hosts may use passwordless sudo or an ordinary sudo password. Mesh
prompts for the password only after consent and validates it before any remote
change. The remote shell consumes the password, validates sudo, and then runs
the listed command with `sudo -n` in the same shell session. Installer data and
the Tailscale auth key remain separate stdin payloads; neither secret appears in
argv. CLI automation therefore needs a root SSH login or suitable passwordless
sudo in addition to `--yes`.

`tailscale up` receives the local key through stdin with
`--auth-key=file:/dev/stdin`. Authentication failures discard remote stdout,
stderr, and the returned error text so a hostile or broken CLI cannot echo the
key into a Mesh error. The auth-key form runs only for `NeedsLogin` and
`NoState`. A `Stopped` backend runs `tailscale up` without an auth key.

SSH stdout and stderr are bounded. Repository files and the fallback installer
have tighter one-MiB download limits, and remote diagnostic text is truncated
to a small bounded excerpt.

On Linux, the same confirmation covers `loginctl enable-linger`. Mesh then runs
`loginctl show-user <user> --property=Linger` and requires `Linger=yes`. The
service installer repeats that check before it writes the Mesh service.

## What already exists

The detection half is complete and this task must not duplicate it:

- `DiagnosticTailscaleUnavailable` and `DiagnosticTailscaleLoggedOut`
  (`errors.go:18`), inside a catalog that already covers arch, systemd, user
  lingering, ports, and clock skew.
- A backend state machine in `discovery.go:52` that classifies `Running`,
  `NeedsLogin`, `NoState`, `Stopped`, `Starting`, and `NeedsMachineAuth`.
- `remote.Run(ctx, command, stdin)` — note the stdin parameter, which this task
  depends on.
- `deps.install`, `deps.discover`, and `deps.verify` are already injected
  function fields, so `deps.provision` follows the established pattern and stays
  testable without a real machine.
- `StepInstall`, `StepDiscover`, `StepVerify` progress events. Add `StepProvision`.

## Responsibilities

1. **Probe before writing anything.** Today `run.go` installs the Mesh service
   at line 111 and only discovers Tailscale at line 124. A host that can never
   get a Tailnet address therefore ends up with a Mesh service on it. Move the
   Tailscale probe up to just after platform detection, so a machine that cannot
   be provisioned fails before anything is written to it.

2. **Provision through the platform package manager first.** Prefer the distro's
   own package (`pacman -S tailscale`, the Tailscale apt repository) over
   piping a script. If a script is unavoidable, fetch it and verify a pinned
   checksum before executing, matching the checksum-gated policy the T10
   installers already enforce. **Never pipe an unverified script into a shell on
   a remote machine.**

3. **Authenticate with an auth key, delivered over stdin.** `tailscale up`
   otherwise needs an interactive browser, which adoption cannot drive. Accept
   `--tailscale-auth-key-file`, read the key locally, and pass it to the remote
   `tailscale up` **through stdin, never as a command-line argument** — argv is
   world-readable in `ps` on the remote host, so a key passed as a flag leaks to
   every local user there. The key must never reach the address book, a progress
   event, an error message, or a log line.

4. **Wait out the transient states.** `Starting` is currently a hard failure at
   `discovery.go:55`, but it is a state that resolves on its own within seconds
   — and it is exactly what a freshly provisioned or freshly booted host reports.
   Poll to a bounded deadline before failing. `Stopped` means the daemon is up
   and the backend is down, which `tailscale up` fixes.

5. **Split the states a human must fix.** `NeedsMachineAuth` means an
   administrator has to approve the machine in the tailnet, and no amount of
   local action changes it. It deserves its own diagnostic code and a message
   naming the admin console, rather than being folded into
   `DiagnosticTailscaleUnavailable` with everything else.

6. **macOS installs the headless package, never the application.**
   `brew install tailscale` is the open-source daemon and CLI, and `sudo brew
   services start tailscale` runs it as a background service. That is what a
   provisioned host gets, on every platform. Mesh never installs the GUI
   application, from the App Store or anywhere else.

   The application still has to be *detected*, because the two cannot coexist:
   the open-source `tailscaled` and the app contend for the same tun interface
   and state, so installing one beside the other breaks the host. If the app is
   already there, do not install a second daemon — drive the CLI inside the
   bundle at `/Applications/Tailscale.app/Contents/MacOS/Tailscale`, report
   which variant the host is on, and continue. Detection is a safety check, not
   a second supported path.

7. **Enable lingering while installing the service.**
   `DiagnosticNoUserLingering` currently names the problem and stops. A systemd
   *user* service runs only while that user has a login session, so without
   `loginctl enable-linger <user>` the daemon is killed the moment the adopting
   SSH connection closes. That breaks invariant 2 on the very first disconnect,
   and it presents as a Mesh bug rather than a missing system setting. Enable it
   as part of the install step, under the same confirmation, and verify with
   `loginctl show-user <user> --property=Linger` rather than trusting an exit
   code. Not applicable on macOS: launchd agents have no equivalent constraint.

8. **Widen the progress column.** `cmd/mesh/bootstrap.go:69` formats steps with
   `%-8s`, and today's longest names are exactly eight characters. `provision`
   is nine and shifts the column on every line it prints. Widen the pad in the
   same change that adds `StepProvision`.

## The part that needs care

**Installing software on someone else's machine is the confirmation-worthy act,
not a detail of it.** Gate it the way T14 gates public exposure: interactive
confirmation by default, `--yes` to skip, and the prompt names the package
manager and the exact command. An unattended `mesh add` in a script must not
silently install a daemon on a remote host because a probe came back empty.

**Adoption stays idempotent.** `mesh add` already returns `AlreadyConfigured`
for an unchanged install, and re-running it is a supported, ordinary thing to
do. Provisioning must preserve that: an already-installed, already-authenticated
Tailscale is a no-op that reports what it found, not a reinstall.

**Do not authenticate a host that is already authenticated.** Running
`tailscale up` with an auth key against a host that is already `Running` can
change its tags or re-register it. Probe first, and only authenticate a backend
that actually reports `NeedsLogin` or `NoState`.

**A failed provision must not install Mesh.** Package installation can partially
change the remote host before a later command fails. In that case, report
whether Tailscale was installed or whether provisioning may have changed the
host. The operator must know whether to retry adoption or only correct the
remaining Tailscale step.

## Acceptance

- `mesh add` against a host with no Tailscale prompts, names the package
  manager and command, and does nothing on refusal — no Mesh service, no
  Tailscale, nothing written.
- `--yes` provisions without prompting. An unattended run with no `--yes` and no
  TTY refuses rather than assuming consent.
- The auth key never appears in argv on the remote host. Assert it by capturing
  the exact command line the fake remote receives, and prove the key arrives on
  stdin instead.
- The auth key never appears in any progress event, error, `Result`, or address
  book entry. Adopt with a key, then grep the entire recorded output for it.
- A host reporting `Starting` is polled to success rather than failed
  immediately, and a host stuck in `Starting` fails at a bounded deadline with a
  message naming the state.
- `Stopped` recovers via `tailscale up`. `NeedsLogin` and `NoState` authenticate
  with a supplied key, and fail with a message naming the missing flag when no
  key is supplied.
- `NeedsMachineAuth` returns its own diagnostic code and names the admin
  console. It is never retried.
- An already-`Running` Tailscale is left completely alone. Assert that
  `tailscale up` is never invoked in that case.
- Re-running a complete `mesh add` still reports `AlreadyConfigured` and changes
  nothing.
- A Darwin target with no Tailscale installs the Homebrew package and starts it
  as a service. Assert the application is never downloaded or installed.
- A Darwin target that already has the GUI application drives the CLI inside the
  bundle, never runs `brew install`, and names the variant in its output. Assert
  no second daemon is started.
- A script fetched for provisioning is checksum-verified before execution, and a
  checksum mismatch aborts without running it.
- Provisioning failure leaves no Mesh service installed, proven by probing the
  remote after the failure.
- Adopting a Linux host without lingering enables it, and
  `loginctl show-user <user> --property=Linger` reports `Linger=yes` afterwards.
- Close the adopting SSH session and the daemon is still running a minute later.
  This is the regression `DiagnosticNoUserLingering` exists to warn about, and
  the one a user would otherwise report as lost sessions.
- A host that already has lingering is left alone and reports no change.

## Verification

Run the retained T20 check:

```bash
./scripts/check-t20.sh
```

The script runs the focused race tests, `go vet`, the systemd and launchd
installer harness, and a static check that rejects any production auth-key flag
except `--auth-key=file:/dev/stdin`.

The repository checks are:

```bash
go mod tidy -diff
golangci-lint run ./...
go test -race ./...
go vet ./...
./scripts/verify.sh
```

The fake SSH remotes cover consent refusal, exact confirmation-to-execution
commands, password and passwordless sudo, stdin separation for both secrets,
successful apt and fallback installs, bounded downloads, error redaction, every
backend state, macOS variant selection, checksum mismatch, partial-install
reporting, pre-install failure ordering, and lingering verification.

Three checks still need real remote hosts before a release:

1. Adopt a Linux host without lingering. Close SSH, wait one minute, and run
   `systemctl --user is-active mesh.service` on a new SSH connection.
2. Adopt one Mac with `Tailscale.app` and one Mac without it. Confirm that the
   first Mac keeps the application variant and the second Mac runs the Homebrew
   service.
3. Adopt a non-root host whose sudo policy requires a password. Confirm that the
   password prompt follows consent and that package installation completes.

## Out of scope

Installing Tailscale on the machine running `mesh add` itself — that is the
operator's own setup, not adoption. Creating, rotating, or storing auth keys;
Mesh reads one from a file and forgets it. Configuring tags, ACLs, exit nodes,
subnet routes, or Tailscale Serve, all of which are tailnet policy rather than
host adoption. Uninstalling Tailscale.
