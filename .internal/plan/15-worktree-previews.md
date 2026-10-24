# Preview a worktree from a link

Status: Approved, 2026-10-02 (moved from Fregat Plan 288 on 2026-10-10).
Scheduled later: start when the owner says Fregat has left its greenfield
phase. Until then feature PRs merge rough and get fixed in the next PR.

Builds on [T28: serve on demand](../tasks/T28-serve-on-demand.md) and
[06: private temporary apps](06-temporary-apps.md). D30 bounds the lifecycle.

## Outcome

An agent that finishes a large feature PR ends its run with a link the owner
can open from a phone or the MacBook. The link runs that PR's worktree: its own
server and web app against its own state. The owner tries the feature, then
merges or replies in the thread. An unopened preview costs disk only, and an
idle one stops and expires on its own.

Any repository can be previewed. Fregat is the first consumer.

## Shape

- **Recipe.** The repository supplies how to run a worktree: a command that
  takes free ports and a state directory as arguments. Mesh owns ports, the
  route, the URL, start, stop and expiry. Nothing in the repository refers to
  Mesh or to the owner's machines.
- **Process.** `mesh preview` registers the worktree's recipe as an on-demand
  route (T28). The first request starts it as a Mesh session; it stops after
  the idle window.
- **State.** Each preview gets a fresh state directory, seeded by the recipe
  from the repository's own fixture, so it never writes into dev or production
  state.
- **Address.** A private temporary app (06) gives the preview a short URL. It
  expires 24 hours after the last admitted visit and Mesh deletes its managed
  files. The worktree stays.
- **Capacity.** A per-host cap on running previews. Starting one past the cap
  stops the least recently used preview first.
- **Agent contract.** The agent posts the link in the thread and in the PR
  body, and keeps the preview alive until merge or close. Settling the thread
  withdraws the route. This contract belongs in the `orchestrate` skill.

## Fregat as the first consumer

Fregat changes these in its own repository, generically, before this plan can
finish:

- One command starts the server and web app on given ports with a given state
  home. Most of this exists; confirm it runs from a fresh worktree.
- A small fixture workspace (a real git repo with a few sessions) seeds a fresh
  state home.
- Feature PRs that change both the server and the web app get a preview. A PR
  that only touches styling or copy deploys normally.

## Before starting

- Measure how long reverting a merged change and redeploying takes. If it is
  more than a minute, fix that first.
- Measure an idle Fregat server's memory to choose the per-host cap.

## Open questions for the owner when this starts

- Which machine hosts previews, and how many may run at once.
- Whether a preview may reach live agent providers or only the mock provider.

## Done when

A feature PR opened by an agent carries a working preview link, the owner
merges from it without running anything locally, and an idle preview stops
and expires on its own.
