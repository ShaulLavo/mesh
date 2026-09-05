# Wake a machine

Use an awake machine on the PC's local network to wake its wired NIC. A Pi can
fill that role. A Mac at home can also send the packet; away from home it asks
an awake Mesh peer on the PC's LAN.

1. Enable Wake-on-LAN in the PC's firmware and operating system. Keep the NIC
   powered during sleep. Disable firmware ErP if it removes NIC standby power.
   Mesh does not change firmware or NIC settings.
2. Start the current Mesh daemon on the PC and at least one machine that stays
   awake on its LAN. Update the clients and senders too.
3. On the PC, run `mesh wake allow`. If Mesh reports no wired network, check its
   default-route interface and gateway neighbor information. Linux needs `ip`;
   macOS uses `route`, `arp`, and `networksetup`.
4. While the PC is online, run `mesh add shaul@pc` from the Mac. If already
   adopted, run `mesh ls` to refresh its permission.
5. Suspend the PC, then run `mesh wake pc` from the Mac. The command reports the
   sender and succeeds when the PC's Mesh daemon responds. Allow up to 90 seconds.

To connect and wake in one operation, run `mesh pc`, `mesh pc -r`, or attach a
known session ID. Listing sessions never wakes a machine. If no sender is
available, leave another Mesh machine awake on the PC's LAN.
Automatic peer discovery uses the standard Mesh port 7337. The CLI also checks
adopted hosts at their configured ports.

To disable permission, run `mesh wake deny` on the PC. Reachable peers learn the
change in the background. A peer that misses the change may retain old permission
until reconnection or its 30-day expiry. Keep the PC online during a permission
change so peers can learn it.

After a gateway change, bring the PC online so Mesh can discover its new LAN.
After more than 30 days offline, bring it online to renew expired permission.
Do not delete the wake policy files to reset permissions.

On Linux, persist the NIC wake setting through your network manager or a systemd
`.link` file with `WakeOnLan=magic`. A machine without mains or standby power
cannot receive a wake packet. Configure the firmware's AC power restoration
behavior separately if it should start after an outage.

The daemon prevents idle sleep while sessions are running, including detached
sessions. Linux needs working `systemd-inhibit` authorization; macOS uses
`caffeinate`. Unavailable inhibition logs once and leaves sessions usable. Forced
sleep, loss of power, and reboot can still interrupt sessions.

For a published service, enable `--wake-on-request` when serving it. The target
must also allow waking, and an awake sender must remain on its LAN. The edge
waits for the target and a fresh service publication before forwarding traffic.
