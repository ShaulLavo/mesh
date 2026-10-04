# Approve a device on selected pinned hosts

Run `mesh device approve-fleet` on the source device that needs access. Each
selected destination receives that source's full-control and SSH grant for its
daemon OS account. A saved pin identifies the destination. An existing system
SSH administration connection authorizes the operation.

Choose existing configured host names or exact host IDs explicitly. Supply an
administration map whose keys exactly match those selections:

```json
{
  "pi": {
    "target": "owner@pi-admin",
    "account": "owner",
    "stateDir": "/home/owner/.local/state/mesh",
    "binary": "/home/owner/staged/mesh"
  }
}
```

`target` is an explicitly chosen system SSH destination or existing SSH Host
alias. `account` is the destination daemon's OS account. `stateDir` is its
existing canonical absolute state path. `binary` is an absolute path to an
already staged, verified native Mesh binary containing these commands. Stage
that binary separately through the existing administration connection.

Preview the selected destinations from the source:

```bash
mesh device approve-fleet --admin-map admin.json --check pi
```

The preview reads the source's existing identity and configured pins. On each
destination it checks the expected account, owned state directory, existing key,
native Unix socket peer account, process state location and daemon identity.
Unavailable process binding fails the check. The state path must resolve to the
connected daemon's state. Preview output reports whether the source already has
an unrestricted device grant and whether approval grants root access.

Grant access explicitly:

```bash
mesh device approve-fleet --admin-map admin.json --yes pi
mesh device approve-fleet --admin-map admin.json --yes --allow-root vps
```

Every selected destination must pass preflight before grants are written.
Applies run serially and repeat the destination checks. The source identity and
selected configured pin are rechecked before each apply. The command stops on a
failure and reports completed and pending destinations. A lost apply receipt
reports an unknown grant outcome. Run `--check` on that destination from the source
to read back its current grant before retrying. Retrying preserves an
existing unrestricted grant and its incarnation. Separate updater administration
policy stays unchanged.

System SSH runs with `BatchMode=yes` and `StrictHostKeyChecking=yes`. Establish
host-key trust through the existing administration procedure before running this
command. The command reads existing keys and configuration. It performs grant
approval through the existing atomic policy writer. Binary staging, service
activation and updates remain separate operations.

The checked destination operation is also available locally through that trusted
administration connection:

```bash
/path/to/staged/mesh device approve-checked \
  --account owner --state-dir /home/owner/.local/state/mesh \
  --destination EXPECTED_DESTINATION_PIN --check -- SOURCE_DEVICE_ID
```

Use `--yes` to apply and add `--allow-root` for a root destination. This command
emits a JSON receipt containing the checked destination, source and full-grant
membership. `approve-fleet` supplies both identities from existing local state
and configuration.

Native Darwin account and environment observation uses kernel process queries.
A restricted or unobservable daemon environment fails closed. Use an observable
native daemon under the intended account and verify this path on the target
platform before rollout.
