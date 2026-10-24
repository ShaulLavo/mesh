# Personal fleet connection sharing

Status: Approved.

## Execution

- [x] Ground local address books, owner names and per-device grants.
- [x] Compare direct pull snapshots with signed push introductions.
- [x] Select explicit mutual personal devices and direct snapshots.
- [x] Implement controller policy, replication, bootstrap grants and CLI lookup.
- [x] Prove fresh offline onboarding, direct commands, revocation and cleanup.
- [ ] Review, merge and deliver through the existing release path.

## Problem

Adding a server saved its destination pin only on the adding device. Another personal device had neither its connection record nor an approved device key at the destination. Renaming it exposed an additional problem: the root command rejected an unknown name before querying already pinned destinations for their current names.

The owner explicitly approved PC and Mac as mutual trusted personal devices on 2026-10-17. Adding a server from either must approve the other. Remote servers remain destinations unless explicitly enrolled as sharing devices.

## Selected design

Each enrolled personal device publishes its own local `hosts.json` snapshot over a direct authenticated control connection. Only a currently approved device key named in the local sharing policy may read it. Receivers store each source's complete snapshot separately and combine it with their local address book. Imported records are never published again.

A complete replacement snapshot carries removals naturally. The local address book is already durable publication, so a second outbox or sequence journal would duplicate state. Failed pulls retain the previous snapshot. The daemon retries at startup and every 30 seconds. The CLI also refreshes an unresolved name, then asks pinned destinations directly for their current names.

`internal/fleetshare` owns policy, safe atomic storage, validation, authenticated exchange and retry. `internal/protocol` carries connection metadata without machine names. The daemon dispatches its controller behind the existing full-control gate. The CLI supplies only the manually adopted host book, merges imported records, and enrolls devices through explicit local commands. Bootstrap installs exactly the selected additional device keys through its existing authenticated SSH installer, with root acknowledgment covering the selected devices.

## Alternatives

Two independent design reviews considered direct snapshots and durable signed push introductions. Both required explicit controller enrollment, per-device destination grants, direct destination name verification and offline delivery.

The direct snapshot design is the base. It avoids signatures for forwarded introductions because no records are forwarded. It avoids push receipts and pending-operation journals because receivers read already durable local host records. The push design's source attribution and explicit separation between trusted devices and destination servers are retained. Initial enrollment runs locally on each device; there is no network RPC that changes sharing policy.

## Verification

Regression tests cover renamed pinned destinations, conflicting destination names, malformed identity replies, explicit enrollment, exact bootstrap key grants and existing installer grant safety. An isolated real-daemon integration adds fresh destinations while another personal device is offline, exercises both directions through normal CLI commands, checks direct execution, then verifies controller and device-grant revocation. Every fixture owns cleanup.
