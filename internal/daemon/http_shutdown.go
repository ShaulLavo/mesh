package daemon

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
)

type newHTTPConnections struct {
	mu           sync.Mutex
	pending      map[net.Conn]struct{}
	shuttingDown bool
}

func trackNewHTTPConnections(server *http.Server) *newHTTPConnections {
	connections := &newHTTPConnections{pending: make(map[net.Conn]struct{})}
	previous := server.ConnState
	server.ConnState = func(connection net.Conn, state http.ConnState) {
		connections.connState(connection, state)
		if previous != nil {
			previous(connection, state)
		}
	}
	// The shutdown flag must precede closing unread requests, so net/http's
	// dispatch check already excludes any request that has not become active.
	server.RegisterOnShutdown(connections.closeNew)
	return connections
}

func (c *newHTTPConnections) connState(connection net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state != http.StateNew {
		delete(c.pending, connection)
		return
	}
	if c.shuttingDown {
		closeNewHTTPConnection(connection)
		return
	}
	c.pending[connection] = struct{}{}
}

func closeNewHTTPConnection(connection net.Conn) {
	// An unread TLS connection needs no close-notify write during shutdown.
	if secure, ok := connection.(*tls.Conn); ok {
		connection = secure.NetConn()
	}
	_ = connection.Close()
}

func (c *newHTTPConnections) closeNew() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shuttingDown = true
	for connection := range c.pending {
		closeNewHTTPConnection(connection)
	}
}
