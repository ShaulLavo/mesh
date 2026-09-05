# Decision log

Decisions that are settled. Each says what we chose and what it costs, so nobody
has to rediscover the tradeoff. Reopen one only with a reason the "cost" line does
not already cover.

## D1 — Sequence numbers are absolute byte offsets

`seq` is the count of PTY bytes emitted since the session started. Resume is a
slice index into the replay ring; "I have everything before N" is the entire
client-side resume state.

**Cost:** output cannot be rebatched retroactively per-chunk, which we never want.
**Alternative rejected:** per-chunk counters, which freeze the original batching
into the protocol forever.

## D2 — One wire format for every transport

`internal/protocol` is transport agnostic: `kind:1 | len:4 | payload`, where data
and input payloads carry `session:8` (and data carries `seq:8`). The Unix socket
and the future WebSocket carry identical frames.

**Why:** it makes the daemon a relay instead of a translator, so step 2 really is
plumbing rather than a rewrite in a plumbing costume.
**Cost:** 8 bytes of session header on every local frame. Irrelevant.

## D3 — Single attacher, steal on attach

A second client attaching evicts the first, which is told `reason: stolen`.

**Cost:** no mirrored terminals, no pair debugging. Revisit when someone actually
wants it; multi-attach forces a resize policy we do not need yet.

## D4 — ctrl+] detaches, and it is configurable

Default `0x1d`, the telnet escape. `--detach-key` accepts `ctrl+]`, `^a`, `none`.
`none` (`--raw`) passes every byte through untouched.

**Why not ctrl+\\:** it is SIGQUIT, i.e. the dump-all-goroutine-stacks button, which
is exactly what you want on a wedged Go daemon.
**Cost:** vim's tag jump inside a Mesh session needs the key rebound, or `none`.

## D5 — Signals travel out of band

`mesh sig <id> <signal>` opens a one-shot control connection that does not attach
and does not disturb the current attacher. Delivered to the process group.

**Why:** it means intercepting any key locally costs the session nothing.

## D6 — `mesh kill` means hangup, then insist

SIGHUP to the process group, 5s grace, close the PTY master, SIGKILL.

**Why not SIGTERM:** interactive shells ignore it, so `kill` would silently do
nothing. Killing a session must always mean the session is gone.

## D7 — The worker records its own death

Each session directory holds `meta.json` with state, exit code, and the kernel
boot ID. The worker writes it; the daemon reconciles it into SQLite on startup.

**Why:** a session's fate must survive the daemon being down, and one SQLite
writer avoids lock contention between many workers.
**Consequence:** state is derived, not stored. Socket answering means `running`;
socket gone with no recorded exit means `interrupted`; boot ID mismatch means the
machine rebooted underneath it.

## D8 — Detachment is `Setsid`, not systemd

The worker is spawned with `SysProcAttr{Setsid: true}`, writes its socket into its
own state dir, and is found again by scanning that directory.

**Why not `systemd-run --user --scope`:** macOS needs the same mechanism, and a
directory scan is one code path for both.

## D9 — No daemon in commit 1

`mesh local` spawns and finds workers directly. The daemon arrives as a
coordinator over a mechanism that already works.

**Why:** it proves invariant 3 by construction. Survival cannot depend on the
daemon if there is no daemon.

## D10 — Cobra and Fang wait for step 3

Commit 1 uses a hand-rolled dispatcher. The real CLI surface is where Cobra earns
its keep; four subcommands do not need a framework.

## D11 — Attach position depends on how you got there

Landed with T02. A brand new session attaches from sequence zero, because a fast
command can finish before the spawn readiness probe returns and its output would
otherwise be lost. Resume and explicit attach keep the bounded tail.

**Cost:** a worker whose command exits before anyone attaches lingers up to 5s
waiting for that first client, then exits on a timer. Scripting many fast
commands leaves short-lived workers behind for a few seconds each.

**Consequence for T01:** the snapshot path replaces the bounded tail, not the
sequence-zero path. A new session has no screen to snapshot.

## D12 — The worker closes its own copy of the PTY slave

`xpty` keeps the parent's slave descriptor open after `Start`. The worker closes
it, so the master reports EOF when the child closes its descriptors.

**Why:** without this, a backgrounded descendant that inherited the slave keeps
the worker alive after the session leader exits, and the session never reports
`exited`. After the leader exits the pump gets 250ms to drain buffered output,
then the worker closes the PTY regardless.

**Cost:** output written by a descendant more than 250ms after the leader exits
is lost. That is the right trade: the alternative is a session that never ends.

## D13 — Two names: `mesh.shaulavo.dev` is private, `shaulavo.dev` is public

`mesh.shaulavo.dev` resolves to tailnet addresses and is reachable only from the
tailnet. Public services are published under `shaulavo.dev` through the VPS edge,
and only when explicitly named.

Certificates work on both. DNS-01 validates by TXT record and never connects to
the host, so the private name gets a real publicly-trusted certificate despite
pointing at unroutable addresses.

**Cost:** tailnet addresses appear in public DNS. They are unroutable from
outside and Tailscale's security does not rest on them being secret, but it is
information published deliberately. Split DNS is the alternative, at the cost of
configuring every client.

## D14 — The VPS is the public edge, and only for serving

Origin machines never open a port to the internet. Public web traffic crosses the
VPS; that is a scoped exception to "traffic goes directly to the destination
machine", which protects terminal sessions.

**Terminal traffic still never transits the VPS**, and T13's acceptance criteria
assert it. The private side has no proxy at all: `pc.mesh.shaulavo.dev` points
straight at that machine.

## D15 — Nothing is public unless you named it

Served services are tailnet-only by default. `--public` takes the hostname
explicitly rather than deriving one, is confirmed interactively, and is refused
when the served root contains anything credential-shaped. Mesh never binds the
`shaulavo.dev` apex unless a service names it.

**Why:** serving a directory is one keystroke from publishing your home folder,
and a derived name is a name nobody read before it went live.

## D16 — Mesh does not serve the personal site

When `shaulavo.dev` gets a site it will be served by something else. The Mesh
edge sits behind that front door: it does not assume port 443, and in the default
arrangement it holds no public certificates, speaking plain HTTP to a proxy that
terminates TLS and sets `X-Forwarded-Proto`.

**Why it matters even though the site does not exist yet:** an edge written
assuming it owns 443 needs restructuring to give it up. Written the other way
round, owning 443 is a configuration choice.

The private side is unaffected. `*.mesh.shaulavo.dev` still needs its own
wildcard certificate via DNS-01, which T12 owns.

## D17 — SSH is a front door for people, not a transport for daemons

Hosts keep talking to each other over `internal/transport`. The SSH server exists
because `ssh` is installed on every machine you will ever sit at and Mesh is not.

**What it buys:** a work laptop you cannot install on, a phone, someone else's
desktop. All of them reach a session with the client they already have.
**Cost:** a second listening server per host, and a second authentication surface
to keep correct.
**Alternative rejected:** SSH as the inter-host transport. T05 already resumes by
sequence number, which is the whole point, and SSH channels would cost that to buy
authentication we already have.

## D18 — One host identity, used by both protocols

The SSH host key is the existing `internal/identity` ed25519 key, exported in
OpenSSH format. Mesh does not generate a second keypair.

**Why:** two keys means two things to trust, two to rotate, and two answers to
"is this really my Pi". Authentication is public key only, and `mesh add` writes
the authorized set during bootstrap, so the machine that adopted a host is the
machine that can reach it.

## D19 — SFTP is the real file server

A browsable HTML listing exists for people holding a browser and nothing else.
SFTP mounts in Finder, Nautilus and Files on Android, with keys already on the
machine.

Both doors serve the roots T11 declared. One declaration, two front doors, no
second notion of what is shared. HTTP is not being replaced; it is being demoted
to the browser case.

**Cost:** SFTP is not a Wish middleware. Wish ships `scp`; SFTP is `pkg/sftp`
wired as a subsystem, which T16 has to build.

## D20 — Reverse tunnels are named and claimed, never arbitrary

`ssh -R` publishes a local port through the VPS edge. This does not reopen
"arbitrary tunnels", which the overview rules out and which stays ruled out.

A forward must be authenticated by an authorized key, bound to a route the user
named explicitly under D15, refused when another host holds that name, and
unpublished the moment the connection drops.

**Why this is worth the exception:** it is the one thing Tailscale cannot do,
because it needs a publicly routable machine, and step 8 already put one there.
**D14 is unaffected.** Tunnelled HTTP is not terminal traffic. Terminal sessions
still go direct.

## D21 — The phone story is SSH, not an app

A phone with the Tailscale app and any SSH client is a complete Mesh client.
Packaging ships nothing for it.

**Why:** an iPhone app was already in "explicitly later" and would have to
reimplement attach, replay, resize and steal against a protocol that changes.
Termius over `ssh` gets all of that by definition. If a native app ever happens
it is a nicety, not the only way in.

## D22 — Serving is exposure, not deployment or sharing

A separate product will handle sharing work with other people: quick dev builds,
preview links, sending someone the thing you are working on. That is not this,
and Mesh will not grow toward it.

Mesh serving answers a narrower and rarer question: *something already runs on
one of my machines and I want to reach it from outside.* An app on the desktop, a
directory on the Pi, a local port. It publishes what exists. It does not build,
version, promote, or hand anything to anyone.

**The test.** If a feature makes sense only because someone else is going to look
at the result, it belongs in the other product. Preview URLs per build, share
links, expiring access, "send this to a colleague", anything that turns a route
into an artifact with a lifecycle.

**Why this matters now rather than later:** T18's reverse tunnel is one command
away from being a sharing tool, and every feature request after it would pull the
same direction. The tunnel exists so *you* can reach your own machine from
outside the tailnet. It is not a way to hand a build to somebody.

**Naming follows from this.** The verb stays `serve`, never `deploy`. `mesh
deploy` would invite exactly the pipeline the previous paragraph rules out.

## D23 — Tailnet transport security belongs to Tailscale

The direct terminal WebSocket and the HTTP service routes on the shared Tailnet
listener are intentionally plaintext. Mesh binds them only to current Tailscale
addresses. Tailscale's WireGuard tunnel supplies confidentiality and integrity,
and Tailnet ACLs supply admission. Mesh destination-identity pinning detects the
wrong daemon, but it is not transport encryption or client authentication.

Canonical private HTTPS is separate. Tailscale Serve forwards raw TCP/443 to the
loopback TLS listener, so Mesh terminates TLS without changing the direct
terminal protocol.

**Cost:** there is no second cryptographic or authorization layer on the direct
listener. A routing or ACL policy that exposes its control port also exposes the
terminal protocol and Tailnet-only HTTP services to those newly admitted
clients. Operators must treat Tailscale routing and ACLs as part of Mesh's
security boundary.

## D24 — Mesh SSH uses port 2222

The operating system's SSH server keeps port 22 for bootstrap and recovery.
Mesh listens on port 2222 at each discovered Tailnet address. The installers
pass `--ssh-port=2222` to the user-owned Mesh service.

**Why:** the system SSH server usually owns wildcard port 22, and a user service
cannot bind a privileged port on a standard Linux host. Replacing the system
server would remove the recovery path that `mesh add` uses.

**Cost:** a stock client needs `-p 2222`. A matching `~/.ssh/config` entry can set the
port, identity file, and `IdentitiesOnly` once.

## D25: Child processes hold sleep inhibitors

The daemon runs `systemd-inhibit` on Linux and `caffeinate -i` on macOS while
the catalog contains running or detached workers. Both wrap `/bin/cat` reading
a pipe held by the daemon, so abrupt daemon death releases the inhibitor.
Missing mechanisms report once and leave session startup available.

**Cost:** extra utility processes while sessions are live, and OS authorization
for Linux inhibition. While the daemon is down, preserved workers lack idle
sleep protection. No logind D-Bus dependency is added.

## D26: Targets permit waking; senders are selected on their LAN

`mesh wake allow` is local target consent for all Mesh machines admitted by
the existing Tailnet boundary. The target signs its NIC, subnet, gateway MAC,
permission, revision, and expiry with its existing identity. Clients carry that
grant to an awake sender, including themselves when on the same LAN.

Match subnet and gateway MAC because different sites can use the same private
subnet. Store grants in separate bounded caches; `hosts.json` stays unchanged.
Explicit wake controls extend the daemon protocol. The old T19 instruction
forbidding protocol changes is superseded.

Explicit connection intent may wake without a witness. Reconnect recovery needs
an independent remote LAN observation. Catalog reads never wake. Replacement
connections discard old buffered input and await the session acknowledgment.

**Cost:** cached permission remains usable for up to 30 days by peers that missed
revocation. Peers retain the highest revision they have seen. Gateway changes
and expiry need rediscovery while the target is awake. ICMP establishes
reachability, not a definitive sleep state. Each sender deduplicates 90-second
attempts; independent senders do not guarantee exactly-once broadcast delivery.

## D27 — ctrl+] belongs to the innermost session; ctrl+^ leaves them all

The outermost client reads every keystroke first, so which client owns a key
is a decision, not an accident. ctrl+] detaches the innermost session. ctrl+^
is consumed by the outermost client and, because every inner client lives
inside that PTY, ends the window with everything inside still attached.

For the outer client to pass ctrl+] through, it has to know an inner client
exists. It learns that live: the inner client holds one extra connection open
to its containing worker for the life of its attachment (`session.nest`), and
the worker tells its attacher when the set changes. A closed connection, for
any reason, means not nested.

**Why not the fixed depth table:** it made ctrl+^ the everyday "leave pc" key
once every window is a session, and ctrl+] just closed the window. Fingers
expect the reverse.
**Cost:** one extra Unix connection per nested client, and a window the length
of one attach round trip in which ctrl+] still reaches the outer client. The
depth table remains as the fallback for workers that predate registration
and for every client nested through that fallback. Attachment requests carry
the shared-key capability of the complete upstream chain. While registrations
exist, a worker rejects an incompatible takeover before changing its attachment
or terminal size; otherwise that takeover would strand the inner clients' keys.
Leave-all is active only while a nested client is registered. Without that
condition, a new outer client would consume a legacy inner client's ctrl+^.
**Changeable:** the leave-all key. ctrl+^ is ctrl+6 on a US layout. It is a
flag, and it was chosen with a shrug.

## D28 — A window is a session, and opening one never steals

Each terminal window attaches exactly one session. Opening a window offers
only detached sessions; attached ones are listed last, never preselected, and
need the picker's explicit take-over. Resuming several sessions means opening
several windows, each of which offers what is still detached.

**Why:** a second window silently yanking the session out of the first is the
one thing that would make people turn the feature off. Hyprland is the
multiplexer; Mesh does not lay out windows.
**Cost:** no mirroring, which D3 already ruled out. A keybind that restores
three sessions spawns three windows with `--take`.
Workers enforce detached-only claims atomically through
`session.attach-detached`, including fresh-window attachment. Old workers reject
that request; `--take` skips them instead of risking a silent takeover.

## D29 — This host comes first and never waits for the network

The picker's first section is this machine, read from session directories and
Unix sockets before any remote query starts. The window prompt is that section
in a compact layout, and it never touches the daemon socket or a remote host.
`l` opens the full picker, which does.

**Why:** a terminal window has to open in the time a shell takes to print its
prompt. Anything that can wait on wifi is not allowed on that path.
**Cost:** this host stays a special case. It is not in its own address book and
will not be added, so `mesh ls` and the picker each splice it in deliberately.
Pre-picker containment capture is local only. Remote containing identities keep
an unavailable frozen preview, because waiting for their screen delays startup
and capturing it after rendering can record the picker itself.
