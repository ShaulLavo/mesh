# Attachment continuity in update tests

Strict same-client resume covers updates whose client and source daemon contain
the reconnect fix. The client process must survive the hop, receive a new listener
challenge, and exchange fresh shell input and output. The fixtures check its
original process and mapped image, the current daemon image, and the retained
session, worker, shell and listener. Losing or replacing the current client fails.

The ordinary `fresh-managed` installed CI job builds two private fixture releases
from the checked-out source, signs and verifies their transition, and updates
between them with one retained current client. It runs on Linux AMD64, Linux ARM64
and macOS ARM64. These fixture versions are private test inputs.

First adoption from the published v249, v255 and v309 pairs has a separate scope.
The retiring daemon can cut authentication, and the old client can exit with
status 0 while its session survives. The fixture records the exit and the owner's
need to reconnect. `strictContinuityPassed` is null on this hop, even when the old
client resumes. Worker, shell, listener, data and update/rollback checks still
apply. A live client that fails its challenge still fails the test.

| Consumer | Attachment scope |
| --- | --- |
| Native published-pair restart control | Historical pair observation; no update proof |
| Adoption, oldest and real draft legacy cases | Legacy first adoption, with exit and reconnect observations |
| Chain, waypoint and ordered fixtures | Legacy bootstrap, explicit reconnect with current bytes, then strict retained-client hops |
| Fresh managed fixture | Strict current-to-current automatic update |
| Fresh real draft | Strict same-client native restart |
| First published updater routes | Strict public baseline-to-target, intermediate and rollback hops |

`installed_native.wait_attachment_resume` always requires its fresh challenge.
`installed_adoption.inspect_retained` first checks preserved components and the
original worker and shell, then applies the fixture's attachment scope. Records
are retained as `*-attachment-continuity.json`; a deliberate reconnect is its own
record and keeps the original session. It is never an automatic retry during a
strict hop. The historical original-client transport diagnostic remains separate
and cannot certify a replacement client's continuity.

`firstPublishedUpdater` remains null until release A is public and verified.
Activation pins that actual release as the permanent baseline. A later real B
release proves public A-to-B continuity. Private fixture evidence makes no claim
that this public path has already passed, and historical failures stay failures.
