# Share Tailnet HTTPS between Mesh and temporary apps

This optional gateway forwards TLS without decrypting it. Tailscale TCP/443
connects to the gateway on `127.0.0.1:8446`. ClientHello SNI selects the backend:

- `apps.shaulavo.dev` and four-character app names go to `127.0.0.1:8445`.
- Other hostnames go to the existing private Mesh listener on `127.0.0.1:8443`.

Build and run the gateway on the Mesh host:

```sh
go build -o /work/mesh/tls-gateway ./examples/tailnet-gateway
/work/mesh/tls-gateway
```

Configure the private Mesh daemon with its existing HTTPS certificate options
and `--https-port=8443 --tailscale-serve --tailscale-serve-port=8446`.
Configure the temporary-app edge with `mode: "direct-tls"`,
`listenAddress: "127.0.0.1:8445"`, and its certificate renewer identity.
The certificate renewer must issue and deliver a certificate covering
`*.shaulavo.dev` to that edge. Existing private Mesh certificates stay on the
private backend. The gateway stores no certificates or browser credentials.

Point the management and app DNS names at the host's Tailnet IP. Clients need
Tailnet access, even for apps whose visibility is public. Pairing only establishes
owner permissions; visitors to public apps do not pair. HTTPS works automatically
for both visitors and owners once routing, DNS, and certificates are configured.

The gateway accepts `--listen`, `--private`, and `--apps` to override these ports.
It bounds the ClientHello size and handshake/connect times. This example is for
Tailnet hosting; it does not configure an internet-facing deployment.
