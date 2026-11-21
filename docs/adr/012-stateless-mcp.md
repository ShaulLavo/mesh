# Mesh speaks only stateless MCP 2026-07-28

Mesh uses revision 2026-07-28 on every MCP hop. The frontend connects directly
to the owning host. Mesh carries the MCP messages as JSON objects over its
authenticated control stream and checks them again on the upstream stdio or
HTTP hop. Durable work belongs to the host. MCP request IDs identify in-flight
messages and application handles identify work across calls. [Pinned specification](https://github.com/modelcontextprotocol/modelcontextprotocol/tree/2f3a95074916f092cd58dc06ede6a748f681a6ef/docs/specification/2026-07-28)

`internal/mcpwire` uses Go's standard library. It checks version and capabilities
on each request and correlates each reply or notification. The stdio transport
uses one JSON object per line and cancellation notifications. Streamable HTTP
validates routing headers before dispatch and uses the request-scoped response
stream for cancellation. Incoming legacy resume and session headers have no
effect. Mesh requires an explicit `resultType` from hosted peers. The pinned
specification's interpretation of an omitted discriminator applies when
decoding a peer that implements an older revision. [Pinned schema](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/2f3a95074916f092cd58dc06ede6a748f681a6ef/schema/2026-07-28/schema.ts)

Mesh keeps JSON values it does not interpret, including tool schema extensions
and vendor fields. Routing may replace an ID or selected metadata keys. The
MCP object has a 3.5 MiB cap. The encoded Mesh control frame also has its own
4 MiB cap, checked in `internal/protocol/mcpframe`. Mesh will keep continuation ownership, authentication and worker
supervision outside the wire package.

## Reuse comparison

Run `go test ./internal/mcpwire -run SDK -v` to replay the comparison. The
positive tests use the official server and client. Independent byte fixtures
define the rejection contract.

| Probe | Guarded Go SDK v1.8.0 | Raw Mesh wire |
| --- | --- | --- |
| Positive stdio and HTTP | The typed client sent `server/discover`, `tools/list`, then two `tools/call` requests. The HTTP trace contained only POSTs at 2026-07-28. | The pinned SDK server returned explicit complete discovery, list and call results on stdio and HTTP. Both HTTP response types passed, `application/json` and `text/event-stream`. |
| Unsupported version | The guard returned HTTP 400 and JSON-RPC `-32022` with `requested: "2025-11-25"` and `supported: ["2026-07-28"]`. An unguarded SDK server restricted to 2026-07-28 accepted a legacy stdio `initialize` and returned `protocolVersion: "2025-11-25"`. The SDK client's version option leaves a legacy fallback path in `Client.Connect`. | The raw fixtures assert legacy stdio initialization fails with `-32601`. A legacy HTTP POST without current headers gets `400/-32020`. A consistently declared old version gets `-32022`. |
| Multi round-trip call | The typed client advertised form elicitation and retried the call with the response to `confirm`. | The first reply had `resultType: "input_required"`, `requestState: "opaque"` and `inputRequests.confirm.method: "elicitation/create"`. The next ID returned a complete reply after the input and state were echoed. The intermediate reply carried no cache hints. |
| Vendor fields | The typed `ListToolsResult` round trip kept `icons` and vendor metadata, and dropped the top-level tool field `x-vendor-shape`. | ID and server metadata rewrites preserved `"x-vendor-shape":{"opaque":900719925474099312345}`. |
| Cancellation and resume | Closing an ordinary SSE response cancelled the SDK handler with `Stateless: true` and `PropagateRequestCancellation: true`. An unguarded stateless SDK POST with `Last-Event-ID` returned HTTP 400. | Fresh loopback connections with legacy headers received the same result as a plain request. Those headers supplied no authorization, and GET and DELETE returned 405. |

The guard already owns the strict protocol checks. Typed SDK forwarding loses
vendor data the Mesh gateway needs to keep. ToolHive v0.67.0 also leaves
legacy backend paths and incomplete modern MRTR. The other candidates in the
design introduce a process or deployment stack, lack evidence for this
revision on every hop, or still need custom integration into Mesh's owning-host
routing and worker model.

## Test dependency

Pin `github.com/modelcontextprotocol/go-sdk` to v1.8.0. The annotated tag object
is `eeddd84db4054984ba4d2d2504851a5001182d53` and the peeled commit is
`3f3b699b2b67e1ed033a63d6651671dab53c2d32`, also reported by
`go list -m -json github.com/modelcontextprotocol/go-sdk@v1.8.0`. The tagged
license is in transition. New code and specification contributions and
consented relicensing use Apache-2.0. Earlier code without relicensing consent
retains its MIT license. Documentation outside the specification uses
CC BY 4.0. Mesh imports the SDK only in `sdk_test.go` to test both directions
against an independent maintained implementation. [Tagged source and license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/LICENSE)

Against Mesh base `f6212be330ab3a5de71ddfd024068ccc251c9efa`, the
`go list -mod=readonly -m all` comparison added four selected modules and changed
no existing selection.

| Added module | Selected version |
| --- | --- |
| `github.com/modelcontextprotocol/go-sdk` | `v1.8.0` |
| `github.com/google/jsonschema-go` | `v0.4.3` |
| `github.com/segmentio/encoding` | `v0.5.4` |
| `github.com/yosida95/uritemplate/v3` | `v3.0.2` |

`go mod tidy` also made the already-selected `github.com/segmentio/asm v1.2.1`
an explicit indirect requirement. The sorted `go list -mod=readonly -deps
./cmd/mesh` comparison showed no package changes. M1 tests the wire packages
directly. The SDK dependency and both wire packages are absent from the
production command graph until the appropriate consumers land.
