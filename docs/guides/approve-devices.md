# Approve a device

`mesh add` approves your current device on a new host. Use these steps to approve another device, or to revoke one. For why approval works this way, read [Device access](../concepts/device-access.md).

An approved device controls the OS account that runs the destination daemon. Approve only devices you own.

## Approve a device on a host

1. On the device you want to approve, print its identity:

   ```bash
   mesh device identity
   ```

   ```text
   gJKYN-Oqs0Son91WIjfOXTV3osI1WmEvVV1yxl-2g54
   account shaul
   fingerprint SHA256:VX33d7r86VCBmq7tfuHKFguStJ0WSn+HWc+tDYPuaZY
   ```

   The first line is the device public ID. Add `--json` for scripts.

2. Open a trusted channel to the destination, either system SSH or its local terminal. Compare the device ID and SHA256 fingerprint on both sides.
3. On the destination, approve the device by its public ID:

   ```bash
   mesh device approve -- DEVICE_PUBLIC_ID
   ```

   If the destination daemon runs as root, add `--allow-root` to acknowledge it:

   ```bash
   mesh device approve --allow-root -- DEVICE_PUBLIC_ID
   ```

For root hosts you can also run `mesh add root@host --allow-root` from the new device.

## Revoke a device

On the destination:

```bash
mesh device revoke -- DEVICE_PUBLIC_ID
```

Sessions keep running. The revoked device cannot attach again, and its `mesh ssh` commands end.

## Approve several hosts or share connections

- To approve this device on many hosts you already pinned, run `mesh device approve-fleet`. See [Approve a device on selected pinned hosts](device-enrollment.md).
- To share saved connections between your own devices, run `mesh fleet trust HOST --yes` on each. See [Share connections between personal devices](fleet-sharing.md).
- A daemon upgrade keeps its workers. To move an existing fleet to authenticated controls, see [Independent local upgrade and device enrollment](updates.md#cross-the-control-authentication-cutover).
