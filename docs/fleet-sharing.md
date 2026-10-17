# Share connections between personal devices

Enroll each device once to share connections automatically. Both devices must already have pinned Mesh connections and approve each other's device keys.

On the PC:

```bash
mesh fleet trust mac --yes
```

On the Mac:

```bash
mesh fleet trust pc --yes
```

Each selected device can introduce servers to the other. When you add a server on either device, Mesh also approves the other device's key on that server:

```bash
mesh add root@server --allow-root
```

`--allow-root` acknowledges root access for this device and every explicitly trusted sharing device. Adding a server does not make that server a trusted sharing device.

Connections arrive through the direct authenticated Mesh connection. A running daemon refreshes them at startup and every 30 seconds. An unknown machine name also triggers a bounded refresh, so a newly added server can be used immediately:

```bash
mesh upcloud-4
```

The destination supplies its current machine name after Mesh verifies its pinned key. Renaming a known machine on another device does not require a separate listing before using the new name.

Retained connections remain available when the other personal device is offline. The adding device must return online before another device can learn a connection it has never received.

To fetch connections explicitly:

```bash
mesh fleet sync
```

To stop accepting connections from a device and stop granting it access during future additions:

```bash
mesh fleet untrust mac --yes
```

This removes connections imported from that device. Locally added connections remain. Existing server grants remain until you revoke that device on those servers with `mesh device revoke DEVICE_ID`.

Sharing existing connection records does not approve new device keys on servers added before enrollment. Use the existing [device enrollment commands](device-enrollment.md) for those servers.

Mesh stores sharing policy and retained connections in `connection-sharing.json` beside `hosts.json`. Local address-book entries take priority over imported records with the same identity. The update fleet's separate `fleet.json` still controls update membership.
