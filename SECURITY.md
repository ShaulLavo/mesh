# Security policy

mesh opens terminal sessions on your machines, so a session is a shell on a host. Reports about who can reach or approve one are welcome.

## Report a vulnerability

Use GitHub's private vulnerability reporting. Open the repository's **Security** tab and choose **Report a vulnerability**, or go to <https://github.com/ShaulLavo/mesh/security/advisories/new>.

Include the commit or version, what you did, what happened, and what you expected. Redact real hostnames, keys, and tokens.

There is no bug bounty and no response-time commitment. This is a personal project.

## Supported versions

mesh is pre-1.0. Only the `main` branch receives fixes.

## Scope

[Device access](docs/concepts/device-access.md) describes how devices are approved, and the README states that the tailnet is the network boundary. A break in either is in scope.
