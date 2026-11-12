# Managed installation defaults

Status: Approved, implementation in progress.

Unit 11 owns the root installer, embedded Linux and macOS installers, local
daemon installation, the missing-policy default, and focused tests. It consumes
policy source `6a10490b` and adoption source
`1ce24815941e12c56c9dcb5565f20616abeceeb5` through ordinary merges.

A fresh managed official installation must verify and start its helper before
the daemon starts. Its absent policy means automatic with no predecessors.
Persisted decisions and policy revisions remain authoritative. Package,
client-only, and development installations retain their separate eligibility.

The activation release requires normal signed publishing CI and Unit 8's real
native installed-pair and chain gates. Local implementation checks do not
authorize publication or a real fleet change.
