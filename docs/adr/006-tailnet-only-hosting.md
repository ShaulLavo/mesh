# 006. Hosting stays inside the tailnet

Status: accepted
Date: 2026-10-08

## Context

Mesh first planned a VPS edge that published named services and temporary apps at public hostnames, with `ssh -R` reverse tunnels claiming routes. That is a scoped exception to "terminal traffic goes host to host", it needs a public edge, certificates and a claim registry, and it pulls Mesh toward sharing work with other people.

## Decision

Mesh serves and hosts only inside your tailnet. Services and temporary apps need HTTPS and a verified Tailnet connection, and apps need owner authorization too. The public edge, public temporary apps, public serving and reverse tunnels are removed. Public hosting belongs to a separate project, Brine, on its own domain. `scripts/check-private-hosting-boundary.sh` fails CI if the removed packages or flags come back.

## Alternatives considered

- **Keep the VPS edge for named public routes.** It shipped, but every later feature request (share links, preview URLs, expiring access) would pull Mesh into a sharing product.

## Consequences

Mesh has one network boundary to reason about. Anyone outside the tailnet cannot reach a Mesh service. Sharing something with another person needs a different tool. Temporary apps (`mesh app`) are private, expire after 24 hours without traffic, and delete their managed files on expiry.
