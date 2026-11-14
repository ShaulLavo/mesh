// Package transporttrace records opt-in, payload-free server lifecycle diagnostics.
package transporttrace

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Stage string

const (
	RequestBegin         Stage = "request.begin"
	RequestEnd           Stage = "request.end"
	RequestCanceled      Stage = "request.canceled"
	UpgradeDone          Stage = "upgrade.done"
	HandshakeBegin       Stage = "handshake.begin"
	HandshakeDone        Stage = "handshake.done"
	AuthorizeAllow       Stage = "authorize.allow"
	AuthorizeDeny        Stage = "authorize.deny"
	GrantFull            Stage = "grant.full"
	GrantUpdate          Stage = "grant.update"
	GrantScoped          Stage = "grant.scoped"
	GrantNone            Stage = "grant.none"
	GrantCurrent         Stage = "grant.current"
	GrantRevoked         Stage = "grant.revoked"
	AuthorizationRevoked Stage = "authorization.revoked"
	AcceptanceDone       Stage = "acceptance.done"
	HandshakeCanceled    Stage = "handshake.canceled"
	HandshakeClose       Stage = "handshake.close"
	HandshakeClosed      Stage = "handshake.closed"
	StreamBegin          Stage = "stream.begin"
	StreamReadEnd        Stage = "stream.read_end"
	StreamWriteEnd       Stage = "stream.write_end"
	StreamCloseBegin     Stage = "stream.close_begin"
	StreamCloseEnd       Stage = "stream.close_end"
	StreamFailed         Stage = "stream.failed"
	StreamAbortBegin     Stage = "stream.abort_begin"
	StreamAbortEnd       Stage = "stream.abort_end"
	HandlerEnd           Stage = "handler.end"
	ServeEnd             Stage = "serve.end"
	AttachRead           Stage = "attach.read"
	AttachedWrite        Stage = "attached.write"
	DaemonCancel         Stage = "daemon.cancel"
	DaemonCanceled       Stage = "daemon.canceled"
	GroupsCloseBegin     Stage = "groups.close_begin"
	GroupsCloseEnd       Stage = "groups.close_end"
	HTTPShutdownBegin    Stage = "http.shutdown_begin"
	HTTPShutdownEnd      Stage = "http.shutdown_end"
	DaemonEnd            Stage = "daemon.end"
)

type recordKey struct{}
type connectionKey struct{}

type Connection struct {
	recorder *Recorder
	id       uint64
	remote   string
	local    string
}

var configured atomic.Pointer[Recorder]

func ProcessContext(ctx context.Context) context.Context {
	if recorder := configured.Load(); recorder != nil {
		return WithRecorder(ctx, recorder)
	}
	return ctx
}

func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, recordKey{}, recorder)
}

func RecorderFrom(ctx context.Context) *Recorder {
	recorder, _ := ctx.Value(recordKey{}).(*Recorder)
	return recorder
}

func From(ctx context.Context) *Connection {
	connection, _ := ctx.Value(connectionKey{}).(*Connection)
	return connection
}

func Request(r *http.Request) (*http.Request, func()) {
	recorder := RecorderFrom(r.Context())
	if recorder == nil {
		return r, func() {}
	}
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	connection := &Connection{recorder: recorder, id: recorder.connections.Add(1), remote: endpoint(r.RemoteAddr)}
	if local != nil {
		connection.local = endpoint(local.String())
	}
	ctx := context.WithValue(r.Context(), connectionKey{}, connection)
	connection.Emit(ctx, RequestBegin, nil)
	var once sync.Once
	return r.WithContext(ctx), func() {
		once.Do(func() { connection.Emit(ctx, RequestEnd, nil) })
	}
}

func Emit(ctx context.Context, stage Stage, err error) { From(ctx).Emit(ctx, stage, err) }

func Process(ctx context.Context, stage Stage, err error) {
	if recorder := RecorderFrom(ctx); recorder != nil {
		defer func() { recorder.recordPanic(recover()) }()
		recorder.emit(event{Stage: stage, Error: errorClass(err), Context: errorClass(ctx.Err())})
	}
}

func Finish(ctx context.Context) {
	if recorder := RecorderFrom(ctx); recorder != nil {
		recorder.Close()
	}
}

func (c *Connection) Emit(ctx context.Context, stage Stage, err error) {
	if c == nil {
		return
	}
	defer func() { c.recorder.recordPanic(recover()) }()
	c.recorder.emit(event{Connection: c.id, Remote: c.remote, Local: c.local,
		Stage: stage, Error: errorClass(err), Context: errorClass(ctx.Err())})
}

func (c *Connection) Session(stage Stage, id string) {
	if c == nil || len(id) != 4 {
		return
	}
	defer func() { c.recorder.recordPanic(recover()) }()
	for _, ch := range id {
		if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return
		}
	}
	c.recorder.emit(event{Connection: c.id, Remote: c.remote, Local: c.local, Stage: stage, Session: id})
}

func endpoint(address string) string {
	host, port, err := net.SplitHostPort(address)
	number, numberErr := strconv.ParseUint(port, 10, 16)
	if err != nil || numberErr != nil || net.ParseIP(host) == nil {
		return ""
	}
	return net.JoinHostPort(net.ParseIP(host).String(), strconv.FormatUint(number, 10))
}

// Never render arbitrary error text: TLS and protocol errors can quote peer input.
func errorClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	default:
		return "other"
	}
}

func timestamp() int64 { return time.Now().UnixNano() }
