# Step 9 — The SSH front door

Mesh already has a transport. This is not that. `internal/transport` carries
Mesh protocol frames between Mesh daemons and stays the way hosts talk to each
other.

SSH is here for the opposite reason: it is the client that already exists on
every machine you will ever sit at. A locked-down work laptop, a phone, someone
else's desktop, a box you have not installed anything on. Mesh cannot be there.
`ssh` already is.

So the SSH server is a *front door for people*, never a transport for daemons.

## The three doors

### Sessions

```
ssh pc.mesh.shaulavo.dev              # picker, then attach
ssh pc.mesh.shaulavo.dev -t 7K3D      # straight into one session
ssh pc.mesh.shaulavo.dev ls           # one-shot, scriptable, no PTY
```

The session handler is the same picker T09 builds, rendered through Wish's
Bubble Tea middleware instead of a local terminal. Detach, steal, replay and
resize behave identically, because underneath it is the same attachment against
the same worker.

**This is also the phone answer.** A phone running the Tailscale app plus any SSH
client is a complete Mesh client today. No iOS build, no Android build, nothing
for packaging to ship. That moves "iPhone app" from a missing feature to a thing
we chose not to need.

### Files

`sftp pi.mesh.shaulavo.dev` mounts a machine's served roots in Finder, Nautilus,
Files on Android, and every file manager anyone actually uses, authenticated with
keys already on the machine.

The important part is that this is not a second feature. T11 decides which roots
are served and to whom. SFTP is a second front door onto exactly those roots:

```
one declaration  ->  HTTP   for browsers      (T11)
                 ->  SFTP   for file managers (T16)
```

A browsable HTML listing is the worst version of a file server. It exists for
people holding a browser and nothing else. SFTP is what you want the rest of the
time.

### Tunnels

An explicit inactive claim reserves the public hostname; `ssh -R` then publishes
a port from wherever you are through the VPS edge:

```bash
mesh_identity="${MESH_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/mesh}/identity.key"
mesh serve claim vps blog.shaulavo.dev
ssh -N -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes -i "$mesh_identity" \
  -R blog.shaulavo.dev:80:localhost:3000 vps.mesh.shaulavo.dev
# a thing running on this machine, reachable from outside, gone on disconnect
```

This is the one capability Tailscale genuinely cannot give you, because it needs
a publicly routable machine, and the VPS is already that machine for step 8.

It is for reaching your own machine from outside the tailnet, in the rare case
where that is what you need. It is not a way to hand a build to someone else;
that is a different product (D22). The distinction is not pedantry: it decides
every feature request that arrives after this ships.

Note what this is not: tunnelled HTTP through the VPS is not terminal traffic, so
D14 survives untouched. The invariant protects terminal sessions, and terminal
sessions still go direct.

## Host key and who gets in

The host key is the existing `internal/identity` ed25519 key, in OpenSSH wire
format. One machine, one identity, used by both the Mesh protocol and SSH. A
second keypair would mean two things to trust, two things to rotate, and two
answers to "is this really my Pi".

Authentication is public key only. No passwords, no keyboard-interactive. The
authorized set is written by `mesh add` (T08) during bootstrap, so the machine
that adopted a host is the machine that can reach it.

T08 authorizes the adopter's Mesh identity, not whichever key OpenSSH happens to
choose from `~/.ssh/id_*`. Stock OpenSSH therefore selects the state-directory
`identity.key` with `-i` and `IdentitiesOnly`, as the tunnel example shows. The
short session examples assume the equivalent `IdentityFile` entry exists in
`~/.ssh/config`. A phone or another client must import an explicitly authorized
key; possessing a Tailscale identity alone is not SSH authorization.

## Where it listens

The tailnet interface, and nothing else. Every door above is reachable only from
the tailnet. The VPS is the sole exception and only for the tunnel endpoint, which
is what it is for.

Wish ships the middleware this needs and we should use all of it rather than
reinvent any of it: `accesscontrol`, `activeterm`, `ratelimiter`, `recover`,
`logging`. `scp` too, which gets `scp` support for free alongside SFTP.

## Scoping the tunnel exception

The overview says step 8 "does not open the door to arbitrary tunnels" and that
line stays true. A remote forward here is:

- authenticated as a known Mesh host identity or an authorized key;
- bound to a complete public hostname the user claimed explicitly, with the same
  confirmation as any other public service under D15;
- refused if that name belongs to another host;
- alive only while the SSH connection is, and unpublished on disconnect.

The claim itself is durable but inactive: it reserves the whole hostname for the
claiming key and serves nothing. The edge returns 404 before the forward, after
disconnect, and whenever the authenticated key does not match. T18 owns this
claim seam; a T14 service is never used as a dummy reservation.

What is still refused: forwarding to an arbitrary destination, claiming a name
nobody asked for, and anything reachable without an authorized key.

## Tasks

| Task | Owns | Blocked by |
|---|---|---|
| T15 SSH front door | `internal/sshd/` | — |
| T16 SFTP and SCP | `internal/sshfs/` | T11, T15 |
| T17 Sessions over SSH | `internal/sshd/session.go` | T09, T15 |
| T18 Reverse tunnels | `internal/tunnel/`, claim adapters | T13, T15 |

T15 is the shared foundation and lands first. The other three are independent of
each other after it.

## What we verified before planning this

`wish@v1.4.7` contains `accesscontrol`, `activeterm`, `bubbletea`, `logging`,
`ratelimiter`, `recover`, `scp`, `comment` and `testsession`. The underlying
`charmbracelet/ssh` has `tcpip.go` implementing both `direct-tcpip` and
`tcpip-forward`, plus agent forwarding, and ships a `ssh-remoteforward` example.

Two corrections to earlier assumptions, recorded so nobody repeats them:

- **SFTP is not a Wish middleware.** Wish ships `scp`. SFTP is `pkg/sftp` wired
  as a subsystem handler on the underlying server. Budget for that in T16.
- **Wish does not help `mesh add`.** Wish is a server. T08 is the client side of
  SSH and stays on `golang.org/x/crypto/ssh`.

## Deliberately not doing

**Local forwarding (`ssh -L`).** Tailscale already does it, better, with no Mesh
involvement.

**The Wish `git` middleware.** That is Soft Serve wearing a Mesh badge. Hosting
repositories is not what this is.

**SSH as the inter-host transport.** T05 exists, works, and resumes by sequence
number. Replacing it with SSH channels would buy authentication we already have
and cost the resume semantics that are the entire point.
