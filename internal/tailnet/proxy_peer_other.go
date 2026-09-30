//go:build !linux

package tailnet

import (
	"errors"
	"net"
	"runtime"
)

type systemPeerUIDLookup struct{}

func ProxyForwarderUIDs() ([]uint32, error) {
	return nil, errors.New("tailnet: Tailnet owner access requires Linux peer socket UID authentication; unsupported on " + runtime.GOOS)
}

func (systemPeerUIDLookup) PeerUID(net.Conn) (uint32, error) {
	_, err := ProxyForwarderUIDs()
	return 0, err
}
