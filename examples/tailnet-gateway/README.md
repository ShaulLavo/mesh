# Share Tailnet HTTPS between Mesh and temporary apps

This optional gateway forwards TLS without decrypting it. Tailscale TCP/443
connects to the gateway on `127.0.0.1:8446`. ClientHello SNI selects the backend using the accepted domains in
[`domains.json`](../../docs/deployment-domains.md):

- `apps.new.example` and four-character app names on every configured domain go to `127.0.0.1:8445`.
- Other hostnames go to the existing private Mesh listener on `127.0.0.1:8443`.

Build and run the gateway on the Mesh host:

```sh
go build -o ./tls-gateway ./examples/tailnet-gateway
./tls-gateway --domains "$HOME/.config/mesh/domains.json" --tailnet-owner-access
```

Configure the private Mesh daemon with its existing HTTPS certificate options
and `--https-port=8443 --tailscale-serve --tailscale-serve-port=8446` plus
`--tailscale-serve-proxy-protocol`.
Configure the temporary-app edge with `mode: "direct-tls"`,
`listenAddress: "127.0.0.1:8445"`, `tailnetOwnerAccess: true`, and its certificate
renewer identity.
The certificate renewer must issue and deliver a certificate covering
`*.new.example` to that edge. Existing private Mesh certificates stay on the
private backend. The gateway stores no certificates or browser credentials.

Point the management and app DNS names at the host's Tailnet IP. Clients need
Tailnet access, even for apps whose visibility is public. Your devices receive
owner controls automatically when their Tailscale account matches the app's
configured origin device. Tagged devices do not identify a person, so browser
pairing remains available for them. Other accounts and internet visitors receive
no automatic owner permissions. HTTPS works for both visitors and owners.

Tailscale Serve supplies the device address through PROXY v1. The gateway consumes
that header and sends it to both TLS backends. The private listener requires
`--tailscale-serve-proxy-protocol` and authenticates the local forwarder before TLS.
It rejects loopback and unspecified client addresses before dispatching a service.
The service proxy replaces caller-supplied forwarding headers with the verified
client address. Without verified metadata, the gateway closes private routes.
The edge checks Tailscale's local device inventory, cached for at most five seconds,
on each request. It does not issue a browser grant that survives leaving the tailnet.
HTTP forwarding headers never identify an owner. All PROXY listeners require a
header and authenticate the loopback forwarder's socket UID through an exact-tuple
Linux `NETLINK_SOCK_DIAG` query. Neither lookup nor startup enumerates the host's
TCP sockets. Only root and the Mesh process UID may forward source addresses.
Run the gateway as the same UID as Mesh. Root covers Tailscale
Serve's `tailscaled` process. Connections from other UIDs, missing headers, and UID
lookup failures are closed before TLS. Local processes under other UIDs cannot
claim a Tailnet source address. Processes under the Mesh UID remain trusted.
Never target these PROXY listeners with a Serve or Funnel TCP forward that does
not supply PROXY metadata. Otherwise, a remote caller could supply a header through
the trusted `tailscaled` socket.

Rebuild and restart this gateway when deploying changes to its authentication.
`mesh update` updates Mesh, not this separately built gateway.

Automatic Tailnet owner access requires Linux on the edge and gateway, with
kernel `INET_DIAG` support and its `tcp_diag` handler. Any sandbox must allow
`NETLINK_SOCK_DIAG`. Startup looks up an owned loopback connection and requires
its UID to match the Mesh process. Missing handlers, blocked diagnostics, and UID
mismatches fail startup instead of silently refusing all traffic. On macOS and
other unsupported platforms, these owner-access settings also fail startup.
Mac origin hosts and routing with manual browser pairing remain supported.

For app routing with manual browser pairing, omit the owner-access settings.
Private routes through this gateway still require verified source metadata; use
`--tailnet-owner-access` on the gateway and `--tailscale-serve-proxy-protocol`
on the private daemon. The app edge must also enable `tailnetOwnerAccess` so
it consumes the gateway’s PROXY headers.

For direct Tailnet TCP/443 forwarding to the private listener on 8443, enable
`--tailscale-serve-proxy-protocol` on the daemon and PROXY v1 in Tailscale Serve.
Private HTTPS requests without authenticated PROXY ingress receive HTTP 403
and never reach private services. Public-edge HTTP forwarding and the direct Tailnet
HTTP/control listener keep their existing trust policies.

Deploy the daemon and rebuilt gateway together. An old gateway sends raw TLS to
the private backend and is rejected by the new PROXY listener; a new gateway
sends PROXY metadata that an old raw-TLS backend rejects. Neither mismatch falls
back to a loopback client address.

The required `--domains` flag selects the same naming policy used by the backends.
During migration it routes both configured domains at once.
The gateway accepts `--listen`, `--private`, and `--apps` to override these ports.
It bounds the ClientHello size and handshake/connect times. This example is for
Tailnet hosting; it does not configure an internet-facing deployment.
