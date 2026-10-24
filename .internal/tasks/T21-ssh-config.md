# T21 — `mesh add` reads `~/.ssh/config`

**Status:** complete · **Blocked by:** T08 · **Owns:**
`internal/bootstrap/ssh_config.go`, `internal/bootstrap/target.go`

## Goal

```bash
mesh add pc          # Host pc in ~/.ssh/config, no user typed
mesh add root@pc     # an explicit user still wins
```

Adoption dialled the target itself and never read the ssh config, so a Host
alias resolved as a DNS name:

```
ERROR  bootstrap ssh_connect: dial SSH pc:22: dial tcp: lookup pc: no such host
```

`ssh pc` worked at the same moment, which made the diagnostic actively
misleading: it told the operator to check a host name and network route that
were both fine.

## The dependency

`github.com/kevinburke/ssh_config` v1.6.0, the standard parser for this format.
It was already in the module graph as an indirect entry, though `go mod why`
reported the main module did not need it, so this promotes it to direct.

Hand-rolling the parse is the alternative and it is worse: `Host` patterns take
globs and negation, `Match` blocks exist, `Include` pulls in other files, and
first-value-wins ordering differs from what a naive reader would do. Getting
any of that subtly wrong sends adoption to the wrong machine.

## Responsibilities

1. **Resolve `HostName`, `User`, and `Port` for the alias as typed.** Nothing
   else. `ProxyJump`, `IdentityFile`, and `IdentitiesOnly` are out of scope.
2. **An explicit value always wins**, matching ssh, where the command line
   overrides the config file. `target` records `explicitUser` and
   `explicitPort` for exactly this.
3. **A bare host is now a valid target.** `parseTarget` no longer demands a
   user, because the config may name one. `@pc` stays malformed: it states an
   empty user, which nothing can supply.
4. **A missing user after resolution is rejected** by `normalizeOptions`, with
   a message naming the alias it could not find a user for.
5. **No config is not an error.** A missing, unreadable, or malformed
   `~/.ssh/config` resolves nothing, so adoption by address keeps working on a
   machine that has never used ssh interactively.

## Acceptance

- `mesh add pc` resolves `HostName`, `User`, and `Port` from the alias.
- `root@pc` keeps `root`; `pc:22` keeps port 22.
- An alias with no `Host` block dials exactly what was typed.
- A nil resolver leaves a target untouched.
- `@pc`, `shaul@`, and a target with a password are still refused.

## Out of scope

`ProxyJump` and `IdentityFile`, both of which imply Mesh reproducing more of
the ssh client than adoption needs. Writing to the ssh config. Honouring
`Include` beyond whatever the parser does on its own.
