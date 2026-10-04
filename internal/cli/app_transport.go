package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"

	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/sshd"
	"golang.org/x/crypto/ssh"
)

type AppRequestFunc func(context.Context, string, appspkg.Request) (appspkg.Result, error)
type appSSHReply struct {
	Result appspkg.Result `json:"result"`
	Error  string         `json:"error,omitempty"`
}
type appTransport struct {
	request func(context.Context, appspkg.Request) (appspkg.Result, error)
	close   func() error
}

func (a *application) openApps(ctx context.Context, hostID string, port uint16) (appTransport, error) {
	if a.dependencies.AppRequest != nil {
		return appTransport{request: func(ctx context.Context, r appspkg.Request) (appspkg.Result, error) {
			return a.dependencies.AppRequest(ctx, hostID, r)
		}, close: func() error { return nil }}, nil
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return appTransport{}, err
	}
	if hostID == "local" {
		socket := daemon.SocketPath(stateDir)
		return appTransport{request: func(ctx context.Context, r appspkg.Request) (appspkg.Result, error) {
			return localAppRequest(ctx, socket, r)
		}, close: func() error { return nil }}, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return appTransport{}, err
	}
	host, err := resolveHostTarget(hosts, hostID)
	if err != nil {
		return appTransport{}, err
	}
	if err := verifyNamedHost(ctx, host, a.dependencies.DialControl); err != nil {
		return appTransport{}, err
	}
	client, err := dialAppSSH(ctx, host, stateDir, port)
	if err != nil {
		return appTransport{}, err
	}
	return appTransport{request: func(ctx context.Context, r appspkg.Request) (appspkg.Result, error) {
		return remoteAppRequest(ctx, client, r)
	}, close: client.Close}, nil
}

func localAppRequest(ctx context.Context, socket string, r appspkg.Request) (appspkg.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	data, err := json.Marshal(r)
	if err != nil {
		return appspkg.Result{}, err
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return appspkg.Result{}, err
	}
	response, err := daemonControlRequest(ctx, socket, protocol.Control{Type: protocol.TypeAppRequest, RequestID: id, App: data})
	if err != nil {
		return appspkg.Result{}, err
	}
	if response.Type == protocol.TypeError {
		return appspkg.Result{}, errors.New(safeRemoteText(response.Message))
	}
	if response.Type != protocol.TypeAppResult {
		return appspkg.Result{}, errors.New("unexpected app response from daemon")
	}
	var result appspkg.Result
	if err = json.Unmarshal(response.App, &result); err != nil {
		return appspkg.Result{}, fmt.Errorf("decode app response: %w", err)
	}
	return result, nil
}

func (a sshApplication) runApp(ctx context.Context, client sshd.Session) (int, error) {
	if client.AppInput == nil {
		return 1, errors.New("app RPC has no input stream")
	}
	decoder := json.NewDecoder(io.LimitReader(client.AppInput, appspkg.ChunkSize*2))
	decoder.DisallowUnknownFields()
	var request appspkg.Request
	if err := decoder.Decode(&request); err != nil {
		return 1, fmt.Errorf("decode app request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return 1, errors.New("app RPC requires one JSON request")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	result, err := localAppRequest(ctx, a.socket, request)
	reply := appSSHReply{Result: result}
	if err != nil {
		reply.Error = safeRemoteText(err.Error())
	}
	if err = json.NewEncoder(client.Out).Encode(reply); err != nil {
		return 1, err
	}
	return 0, nil
}

func appHostKeyCallback(expected string) (ssh.HostKeyCallback, error) {
	key, err := base64.RawURLEncoding.DecodeString(expected)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("app SSH requires the adopted host's pinned Mesh identity")
	}
	public, err := ssh.NewPublicKey(ed25519.PublicKey(key))
	if err != nil {
		return nil, err
	}
	return ssh.FixedHostKey(public), nil
}
func dialAppSSH(ctx context.Context, host HostRecord, stateDir string, port uint16) (*ssh.Client, error) {
	callback, err := appHostKeyCallback(host.MeshIdentity)
	if err != nil {
		return nil, err
	}
	_, key, err := identity.LoadPrivate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("load Mesh SSH identity: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(host.Endpoint)
	if err != nil || endpoint.Hostname() == "" {
		return nil, errors.New("app host has an invalid endpoint")
	}
	address := net.JoinHostPort(endpoint.Hostname(), strconv.Itoa(int(port)))
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect app host %s: %w", HostLabel(host), err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, chans, requests, err := ssh.NewClientConn(raw, address, &ssh.ClientConfig{User: "mesh", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("authenticate app host %s: %w", HostLabel(host), err)
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, chans, requests), nil
}
func remoteAppRequest(ctx context.Context, client *ssh.Client, r appspkg.Request) (appspkg.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stopConnection := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopConnection()
	data, err := json.Marshal(r)
	if err != nil {
		return appspkg.Result{}, err
	}
	session, err := client.NewSession()
	if err != nil {
		return appspkg.Result{}, err
	}
	defer session.Close() //nolint:errcheck // RPC result decides the outcome
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	session.Stdin = bytes.NewReader(append(data, '\n'))
	var out, stderr appLimitedBuffer
	out.limit = appspkg.ChunkSize * 2
	stderr.limit = 4096
	session.Stdout = &out
	session.Stderr = &stderr
	if err = session.Run("app"); err != nil {
		return appspkg.Result{}, fmt.Errorf("app SSH request: %w: %s", err, safeRemoteText(stderr.String()))
	}
	var reply appSSHReply
	if err = json.Unmarshal(out.Bytes(), &reply); err != nil {
		return appspkg.Result{}, fmt.Errorf("decode app SSH response: %w", err)
	}
	if reply.Error != "" {
		return appspkg.Result{}, errors.New(safeRemoteText(reply.Error))
	}
	return reply.Result, nil
}

type appLimitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *appLimitedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("app transfer exceeds byte limit")
	}
	return b.Buffer.Write(data)
}
