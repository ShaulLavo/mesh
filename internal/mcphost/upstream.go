// Package mcphost runs MCP servers on the machine that owns them. The daemon
// stores their definitions, supervises their processes through worker-owned
// pipes, probes them for the stateless 2026-07-28 protocol, and routes each
// authorized caller's requests to the right instance with host-assigned IDs.
package mcphost

import (
	"context"
	"errors"
	"net/http"

	"github.com/shaul/mesh/internal/mcpwire"
)

// Upstream carries requests to one running MCP server instance. The router
// owns identifiers: every request passed to Call already has a host-assigned
// JSON-RPC ID and, when the caller asked for progress, a host-assigned
// progress token. An upstream never sees a caller's own IDs, and the router
// restores them on the way back.
//
// The stdio upstream multiplexes all calls over the worker's framed duplex
// line attach. The HTTP upstream (H5) sends one POST per call.
type Upstream interface {
	// Call sends request and passes its correlated events to sink in order.
	// Each event has already been validated against request with
	// mcpwire.Exchange, so sink sees zero or more notifications and then one
	// terminal response. Call returns nil after the terminal event was
	// delivered, or the first error from the transport or from sink.
	//
	// Cancelling ctx cancels the exact upstream request: stdio sends
	// notifications/cancelled with the host-assigned ID, HTTP closes the
	// response stream. Nothing is passed to sink after ctx is done.
	//
	// sink runs on the goroutine that called Call. It may block; a slow sink
	// never stalls the upstream's reader or another call, because each
	// upstream bounds its per-call queue and fails only the slow call with
	// ErrSlowConsumer.
	Call(ctx context.Context, request mcpwire.Message, sink func(mcpwire.Event) error) error
	// Done closes when the upstream can no longer carry requests.
	Done() <-chan struct{}
	// Err reports why Done closed; it is nil while the upstream is usable.
	Err() error
	// Close releases the connection. It never stops an owned process.
	Close() error
}

// Errors an Upstream returns from Call. Wrap them with %w so the router can
// classify the outcome for the caller.
var (
	// ErrNotSent means the request never reached the upstream, so the caller
	// may safely try again.
	ErrNotSent = errors.New("mcp upstream: request not sent")
	// ErrOutcomeUnknown means the request may have reached the upstream but no
	// terminal response arrived. The router never replays it.
	ErrOutcomeUnknown = errors.New("mcp upstream: outcome unknown")
	// ErrSlowConsumer ends one call whose queued replies exceeded its budget.
	ErrSlowConsumer = errors.New("mcp upstream: caller is not reading replies")
	// ErrHeaderMismatch reports an HTTP upstream that rejected the mirrored
	// parameter headers it was sent. The router may refresh the tool schema
	// once and retry within the same revision, generation and account.
	ErrHeaderMismatch = errors.New("mcp upstream: parameter header mismatch")
)

// HTTPUpstreamConfig describes one loopback Streamable HTTP endpoint.
// Header values are already read from their secret files; the upstream sends
// them only to URL and never logs them.
type HTTPUpstreamConfig struct {
	// URL is the exact approved endpoint, verified to be loopback.
	URL string
	// Headers are the configured credential headers for this account scope.
	Headers http.Header
	// ToolHeaders returns the compiled x-mcp-header mirroring rules for one
	// tool from the account's current schema, and false when the tool is not
	// known yet.
	ToolHeaders func(tool string) (mcpwire.ParameterHeaders, bool)
}

// newHTTPUpstream is set by the H5 client's own file. Definitions with a URL
// are refused while it is nil.
var newHTTPUpstream func(HTTPUpstreamConfig) (Upstream, error)

// ErrHTTPUnavailable refuses URL definitions in a build without the H5 client.
var ErrHTTPUnavailable = errors.New("mcp: HTTP upstreams are not available in this build")

// DialHTTP opens an HTTP upstream through the registered H5 client.
func DialHTTP(cfg HTTPUpstreamConfig) (Upstream, error) {
	if newHTTPUpstream == nil {
		return nil, ErrHTTPUnavailable
	}
	return newHTTPUpstream(cfg)
}
