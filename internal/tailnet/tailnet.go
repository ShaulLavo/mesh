// Package tailnet discovers the local machine and its peers through Tailscale.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
)

var (
	// These errors let callers select a recovery path without matching text.
	ErrNotInstalled = errors.New("tailscale is not installed")
	ErrNotRunning   = errors.New("tailscale is not running")
	ErrNotLoggedIn  = errors.New("tailscale is logged out")
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

// Client reads Tailscale status through an injected command runner.
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

func (c *Client) status(ctx context.Context) (rawStatus, error) {
	contents, stderr, err := c.runner.Run(ctx, "tailscale", "status", "--json")
	if err != nil {
		return rawStatus{}, explainCommandError(err, contents, stderr)
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

func explainCommandError(runErr error, stdout, stderr []byte) error {
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return fmt.Errorf("run tailscale status --json: %w", runErr)
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
		return fmt.Errorf("run tailscale status --json: %w", runErr)
	}
	return fmt.Errorf("run tailscale status --json: %w: %s", runErr, detail)
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
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return stdout, stderr.Bytes(), err
}
