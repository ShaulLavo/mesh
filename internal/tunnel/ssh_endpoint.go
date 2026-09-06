package tunnel

import (
	"context"
	"io"
	"net"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

const (
	// The pinned SSH implementation allows a 2 MiB data window and sixteen
	// queued 256 KiB request packets. Reserve another 2 MiB for HTTP headers
	// and 4 MiB for in-progress packets, transport and copy buffers.
	channelByteAllowance     = 12 << 20
	MaximumBytesPerForward   = 48 << 20
	maximumStreamsPerForward = MaximumBytesPerForward / channelByteAllowance
	streamCopyBuffer         = 32 << 10
)

type sshEndpoint struct {
	connection *forwardConnection
	hostname   string
	global     chan struct{}
	slots      chan struct{}
	mu         sync.Mutex
	closed     bool
	streams    map[*sshStream]struct{}
}

type sshStream struct {
	net.Conn
	bridge    net.Conn
	channel   gossh.Channel
	closeOnce sync.Once
	retired   chan struct{}
	closeSent chan struct{}
}

func newSSHEndpoint(connection *forwardConnection, hostname string, global chan struct{}) *sshEndpoint {
	return &sshEndpoint{connection: connection, hostname: hostname, global: global,
		slots: make(chan struct{}, maximumStreamsPerForward), streams: make(map[*sshStream]struct{})}
}

func (e *sshEndpoint) reserve() bool {
	select {
	case e.slots <- struct{}{}:
	default:
		return false
	}
	select {
	case e.global <- struct{}{}:
		return true
	default:
		<-e.slots
		return false
	}
}

func (e *sshEndpoint) unreserve() {
	<-e.slots
	<-e.global
}

func (e *sshEndpoint) Dial(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !e.reserve() {
		return nil, ErrCapacity
	}
	result := make(chan dialResult)
	go e.open(ctx, result)
	select {
	case opened := <-result:
		return opened.conn, opened.err
	case <-ctx.Done():
		// OpenChannel has no context. Deactivate before closing SSH to unblock it.
		e.connection.stop()
		return nil, ctx.Err()
	}
}

type dialResult struct {
	conn net.Conn
	err  error
}

func (e *sshEndpoint) open(ctx context.Context, result chan<- dialResult) {
	stream, err := e.openStream()
	if err != nil {
		e.unreserve()
	}
	select {
	case result <- dialResult{conn: stream, err: err}:
	case <-ctx.Done():
		if stream != nil {
			_ = stream.Close()
		}
	}
}

func (e *sshEndpoint) openStream() (*sshStream, error) {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	payload := gossh.Marshal(&forwardedChannel{DestAddr: e.hostname, DestPort: 80, OriginAddr: "127.0.0.1", OriginPort: 0})
	channel, requests, err := e.connection.conn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return nil, err
	}
	local, bridge := net.Pipe()
	stream := &sshStream{Conn: local, bridge: bridge, channel: channel, retired: make(chan struct{}), closeSent: make(chan struct{})}
	go stream.drainRequests(requests)
	e.mu.Lock()
	closed = e.closed
	e.streams[stream] = struct{}{}
	e.mu.Unlock()
	if closed {
		_ = stream.Close()
	}
	go e.pump(stream)
	return stream, nil
}

func (e *sshEndpoint) pump(stream *sshStream) {
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		_, _ = io.CopyBuffer(stream.channel, stream.bridge, make([]byte, streamCopyBuffer))
		_ = stream.Close()
	}()
	_, _ = io.CopyBuffer(stream.bridge, stream.channel, make([]byte, streamCopyBuffer))
	_ = stream.Close()
	pumps.Wait()
	<-stream.retired
	<-stream.closeSent
	e.mu.Lock()
	delete(e.streams, stream)
	e.mu.Unlock()
	e.unreserve()
}

func (s *sshStream) drainRequests(requests <-chan *gossh.Request) {
	gossh.DiscardRequests(requests)
	// x/crypto closes incomingRequests only after removing the channel from
	// its channel table, or when the whole SSH connection terminates.
	close(s.retired)
}

func (e *sshEndpoint) Close() error {
	e.mu.Lock()
	e.closed = true
	streams := make([]*sshStream, 0, len(e.streams))
	for stream := range e.streams {
		streams = append(streams, stream)
	}
	e.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}
	return nil
}

func (s *sshStream) Close() error {
	s.closeOnce.Do(func() {
		_ = s.Conn.Close()
		_ = s.bridge.Close()
		// Packet writes can block on a partition. Budget stays reserved until
		// this write finishes, both pumps stop, and SSH retires the channel.
		go func() { _ = s.channel.Close(); close(s.closeSent) }()
	})
	return nil
}
