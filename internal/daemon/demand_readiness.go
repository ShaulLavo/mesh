package daemon

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	demandPollInterval     = 100 * time.Millisecond
	demandProbeTimeout     = 250 * time.Millisecond
	demandOutputTimeout    = 5 * time.Second
	demandFailureLines     = 20
	demandFailureLineBytes = 240
)

func dialUpstream(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, demandProbeTimeout)
	defer cancel()
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
}

// awaitReady waits until every upstream of service accepts, the session
// ends, or the ready timeout passes.
func (m *demandManager) awaitReady(ctx context.Context, service meshserve.Service, id string) error {
	deadline := time.Now().Add(service.Demand.ReadyTimeout)
	ports := service.UpstreamPorts()
	for {
		if code, ended := m.sessions.sessionExit(id); ended {
			return m.startFailure(ctx, service, id, exitDescription(code), false)
		}
		if m.allAccept(ctx, ports) {
			return nil
		}
		if !time.Now().Before(deadline) {
			reason := fmt.Sprintf("%s did not accept within %s", portList(ports), service.Demand.ReadyTimeout)
			return m.startFailure(ctx, service, id, reason, true)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("route %s: %w", service.Route(), ctx.Err())
		case <-time.After(m.poll):
		}
	}
}

func (m *demandManager) allAccept(ctx context.Context, ports []string) bool {
	for _, port := range ports {
		if m.dial(ctx, net.JoinHostPort("127.0.0.1", port)) != nil {
			return false
		}
	}
	return true
}

// startFailure builds the owner diagnostic. The failed transition logs it under
// a reference; waiting HTTP callers receive only a generic answer and that reference.
func (m *demandManager) startFailure(ctx context.Context, service meshserve.Service, id, reason string, stop bool) error {
	// A worker that accepts the request and never answers must not keep the
	// route starting forever.
	tailCtx, cancelTail := context.WithTimeout(ctx, demandOutputTimeout)
	tail := m.sessions.outputTail(tailCtx, id)
	cancelTail()
	ended := true
	if stop {
		stopCtx, cancel := context.WithTimeout(m.ctx, demandStopTimeout)
		if err := m.sessions.stopSession(stopCtx, id); err != nil {
			reason += fmt.Sprintf("; stopping it failed: %v", err)
			ended = false
		} else {
			reason += "; stopped it"
		}
		cancel()
	}
	message := fmt.Sprintf("route %s did not start: %s (session %s, command %q)", service.Route(), reason, id, service.Demand.Command)
	if tail != "" {
		message += "\n\nlast output:\n" + tail
	}
	return demandFailure{summary: fmt.Sprintf("%s (session %s)", reason, id), message: message, ended: ended}
}

// demandFailure keeps the one-line summary `mesh serve ls` shows apart from
// the full diagnostic retained in the owner log and local control responses.
// ended records that the failed start's session is known to be over, which
// is what lets the route launch another.
type demandFailure struct {
	summary, message string
	ended            bool
}

func (f demandFailure) Error() string { return f.message }

func exitDescription(code *int) string {
	if code == nil {
		return "the session ended without an exit status"
	}
	return fmt.Sprintf("the command exited with status %d", *code)
}

func portList(ports []string) string {
	if len(ports) == 1 {
		return "port " + ports[0]
	}
	return "ports " + strings.Join(ports, ", ")
}

// lastLines keeps the end of a session's output as plain text: the last
// non-blank lines, each cut to a readable width.
func lastLines(text string, count, width int) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		// A bare carriage return redraws the line; only what was left counts.
		if index := strings.LastIndexByte(line, '\r'); index >= 0 {
			line = line[index+1:]
		}
		line = strings.TrimRight(strings.ToValidUTF8(line, "?"), " \t")
		if line == "" {
			continue
		}
		if len(line) > width {
			cut := width
			for cut > 0 && !isRuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
