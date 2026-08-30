// Package tailnet discovers the local machine and its peers through Tailscale.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	statusOutputMaximum = 2 << 20
	statusErrorMaximum  = 64 << 10
	commandWaitDelay    = time.Second
)

var (
	// These errors let callers select a recovery path without matching text.
	ErrNotInstalled = errors.New("tailscale is not installed")
	ErrNotRunning   = errors.New("tailscale is not running")
	ErrNotLoggedIn  = errors.New("tailscale is logged out")
	// ErrCommandOutputTooLarge reports a tailscale process whose output crossed
	// the fixed status boundary.
	ErrCommandOutputTooLarge = errors.New("tailscale command output is too large")
)

// Peer is one machine visible on the current tailnet.
type Peer struct {
	Name   string
	Addrs  []string
	Online bool
}

// CommandRunner runs an external command without mixing status JSON and diagnostics.
type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) (stdout, stderr []byte, err error)
}

// Client reads Tailscale node and Serve status through an injected command runner.
type Client struct {
	runner CommandRunner
}

// NewClient creates a Tailscale discovery client.
func NewClient(runner CommandRunner) *Client {
	return &Client{runner: runner}
}

// Self returns the local Tailscale peer using the installed CLI.
func Self(ctx context.Context) (Peer, error) {
	return NewClient(execRunner{}).Self(ctx)
}

// Peers returns the other peers using the installed CLI.
func Peers(ctx context.Context) ([]Peer, error) {
	return NewClient(execRunner{}).Peers(ctx)
}

// VerifyServeForward checks that persistent raw Tailnet TCP/443 traffic reaches
// the given loopback port without HTTP, TLS, or PROXY protocol handling.
func VerifyServeForward(ctx context.Context, localPort uint16) error {
	return NewClient(execRunner{}).VerifyServeForward(ctx, localPort)
}

// Self returns the local Tailscale peer.
func (c *Client) Self(ctx context.Context) (Peer, error) {
	status, err := c.status(ctx)
	if err != nil {
		return Peer{}, err
	}
	if status.Self == nil {
		return Peer{}, errors.New("tailscale status does not identify this machine; run \"tailscale up\"")
	}
	peer, err := parsePeer("self", status.Self)
	if err != nil {
		return Peer{}, err
	}
	return peer, nil
}

// Peers returns all other peers, ordered by MagicDNS name.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	status, err := c.status(ctx)
	if err != nil {
		return nil, err
	}

	peers := make([]Peer, 0, len(status.Peer))
	for key, raw := range status.Peer {
		if raw == nil {
			return nil, fmt.Errorf("parse tailscale peer %s: empty peer status", key)
		}
		peer, err := parsePeer(key, raw)
		if err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		return strings.Join(peers[i].Addrs, "\x00") < strings.Join(peers[j].Addrs, "\x00")
	})
	return peers, nil
}

// VerifyServeForward checks the installed Tailscale Serve configuration.
func (c *Client) VerifyServeForward(ctx context.Context, localPort uint16) error {
	if ctx == nil {
		return errors.New("tailnet: verify Tailscale Serve with nil context")
	}
	if localPort == 0 {
		return errors.New("tailnet: verify Tailscale Serve with zero local port")
	}

	contents, stderr, err := c.runner.Run(ctx, "tailscale", "serve", "status", "--json")
	if len(contents) > statusOutputMaximum || len(stderr) > statusErrorMaximum {
		return ErrCommandOutputTooLarge
	}
	if err != nil {
		return explainCommandError("tailscale serve status --json", err, contents, stderr)
	}

	var status rawServeStatus
	if err := json.Unmarshal(contents, &status); err != nil {
		return fmt.Errorf("parse tailscale serve status: %w", err)
	}
	if err := validatePrivateServeExposure(status); err != nil {
		return err
	}
	handler := status.TCP["443"]
	if handler == nil {
		return errors.New("tailscale serve TCP/443 is not configured")
	}
	if handler.HTTP || handler.HTTPS || handler.TerminateTLS != "" {
		return errors.New("tailscale serve TCP/443 terminates HTTP or TLS; Mesh requires a raw TCP forward")
	}
	if handler.ProxyProtocol != 0 {
		return fmt.Errorf("tailscale serve TCP/443 enables PROXY protocol version %d; Mesh requires an unmodified raw TCP forward", handler.ProxyProtocol)
	}
	wantTarget := fmt.Sprintf("127.0.0.1:%d", localPort)
	if handler.TCPForward != wantTarget {
		return fmt.Errorf("tailscale serve TCP/443 forwards to %q, want %q", handler.TCPForward, wantTarget)
	}
	return nil
}

type rawStatus struct {
	BackendState string              `json:"BackendState"`
	Self         *rawPeer            `json:"Self"`
	Peer         map[string]*rawPeer `json:"Peer"`
}

type rawPeer struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
}

type rawServeStatus struct {
	TCP         map[string]*rawServeTCPHandler `json:"TCP"`
	AllowFunnel map[string]bool                `json:"AllowFunnel"`
	Foreground  map[string]*rawServeStatus     `json:"Foreground"`
}

type rawServeTCPHandler struct {
	HTTP          bool   `json:"HTTP"`
	HTTPS         bool   `json:"HTTPS"`
	TCPForward    string `json:"TCPForward"`
	TerminateTLS  string `json:"TerminateTLS"`
	ProxyProtocol int    `json:"ProxyProtocol"`
}

func validatePrivateServeExposure(status rawServeStatus) error {
	if endpoint, err := enabledFunnel443(status.AllowFunnel); err != nil {
		return err
	} else if endpoint != "" {
		return fmt.Errorf("tailscale Funnel exposes TCP/443 through %q; Mesh private HTTPS must remain Tailnet-only", endpoint)
	}
	for session, foreground := range status.Foreground {
		if foreground == nil {
			continue
		}
		if foreground.TCP["443"] != nil {
			return fmt.Errorf("tailscale foreground session %q shadows the persistent TCP/443 forward", session)
		}
		if endpoint, err := enabledFunnel443(foreground.AllowFunnel); err != nil {
			return fmt.Errorf("tailscale foreground session %q: %w", session, err)
		} else if endpoint != "" {
			return fmt.Errorf("tailscale foreground session %q exposes TCP/443 through Funnel endpoint %q", session, endpoint)
		}
	}
	return nil
}

func enabledFunnel443(entries map[string]bool) (string, error) {
	for endpoint, enabled := range entries {
		if !enabled {
			continue
		}
		_, portText, err := net.SplitHostPort(endpoint)
		if err != nil {
			return "", fmt.Errorf("enabled Tailscale Funnel endpoint %q is invalid: %w", endpoint, err)
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil {
			return "", fmt.Errorf("enabled Tailscale Funnel endpoint %q has an invalid port: %w", endpoint, err)
		}
		if port == 443 {
			return endpoint, nil
		}
	}
	return "", nil
}

func (c *Client) status(ctx context.Context) (rawStatus, error) {
	contents, stderr, err := c.runner.Run(ctx, "tailscale", "status", "--json")
	if len(contents) > statusOutputMaximum || len(stderr) > statusErrorMaximum {
		return rawStatus{}, ErrCommandOutputTooLarge
	}
	if err != nil {
		return rawStatus{}, explainCommandError("tailscale status --json", err, contents, stderr)
	}

	var status rawStatus
	if err := json.Unmarshal(contents, &status); err != nil {
		return rawStatus{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	switch status.BackendState {
	case "Running":
		return status, nil
	case "Stopped":
		return rawStatus{}, fmt.Errorf("%w: run \"tailscale up\"", ErrNotRunning)
	case "NeedsLogin", "NoState":
		return rawStatus{}, fmt.Errorf("%w: run \"tailscale up\" to log in", ErrNotLoggedIn)
	case "NeedsMachineAuth":
		return rawStatus{}, errors.New("tailscale is waiting for machine approval; approve this machine in the Tailscale admin console")
	case "Starting":
		return rawStatus{}, fmt.Errorf("%w: Tailscale is still starting; try again shortly", ErrNotRunning)
	case "":
		return rawStatus{}, errors.New("parse tailscale status: BackendState is missing")
	default:
		return rawStatus{}, fmt.Errorf("parse tailscale status: unsupported BackendState %q", status.BackendState)
	}
}

func explainCommandError(command string, runErr error, stdout, stderr []byte) error {
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return fmt.Errorf("run %s: %w", command, runErr)
	}
	if errors.Is(runErr, ErrCommandOutputTooLarge) {
		return fmt.Errorf("run %s: %w", command, ErrCommandOutputTooLarge)
	}

	var missing *exec.Error
	if errors.As(runErr, &missing) && errors.Is(missing.Err, exec.ErrNotFound) {
		return fmt.Errorf("%w; install Tailscale from https://tailscale.com/download", ErrNotInstalled)
	}

	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(stdout))
	}
	lower := strings.ToLower(string(stderr) + " " + string(stdout) + " " + runErr.Error())
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "logged out") || strings.Contains(lower, "needslogin") {
		return fmt.Errorf("%w: run \"tailscale up\" to log in", ErrNotLoggedIn)
	}
	if strings.Contains(lower, "failed to connect to local tailscale") ||
		strings.Contains(lower, "tailscaled.sock") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "is tailscale running") {
		return fmt.Errorf("%w; start Tailscale and try again", ErrNotRunning)
	}
	if detail == "" {
		return fmt.Errorf("run %s: %w", command, runErr)
	}
	return fmt.Errorf("run %s: %w: %s", command, runErr, detail)
}

func parsePeer(source string, raw *rawPeer) (Peer, error) {
	name := strings.TrimSuffix(raw.DNSName, ".")
	if name == "" {
		name = raw.HostName
	}
	if name == "" {
		return Peer{}, fmt.Errorf("parse tailscale peer %s: DNSName and HostName are empty", source)
	}

	addrs := make([]string, len(raw.TailscaleIPs))
	for i, text := range raw.TailscaleIPs {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			return Peer{}, fmt.Errorf("parse tailscale peer %s: invalid Tailscale address %q: %w", source, text, err)
		}
		addrs[i] = addr.String()
	}
	return Peer{Name: name, Addrs: addrs, Online: raw.Online}, nil
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // production callers fix the command to Tailscale; tests inject bounded runner fixtures
	stdout := newCappedBuffer(statusOutputMaximum)
	stderr := newCappedBuffer(statusErrorMaximum)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = commandWaitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	} else if stdout.truncated || stderr.truncated {
		err = ErrCommandOutputTooLarge
	}
	return stdout.bytes(), stderr.bytes(), err
}

type cappedBuffer struct {
	contents  []byte
	maximum   int
	truncated bool
}

func newCappedBuffer(maximum int) *cappedBuffer {
	return &cappedBuffer{maximum: maximum}
}

func (b *cappedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	remaining := b.maximum - len(b.contents)
	if remaining < len(contents) {
		b.truncated = true
		contents = contents[:max(remaining, 0)]
	}
	b.contents = append(b.contents, contents...)
	return written, nil
}

func (b *cappedBuffer) bytes() []byte {
	return b.contents
}
