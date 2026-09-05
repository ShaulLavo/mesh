package wake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"
)

const Cooldown = 90 * time.Second

type State string

const (
	Unknown   State = "unknown"
	Reachable State = "reachable"
	Down      State = "down"
)

type SenderOptions struct {
	Discover func(context.Context) (NIC, error)
	Observe  func(context.Context, NIC) (State, error)
	Transmit func(context.Context, NIC, string) error
	Now      func() time.Time
}

type Sender struct {
	cache   *Cache
	options SenderOptions
}

func NewSender(stateDir string) (*Sender, error) {
	return NewSenderWithOptions(stateDir, SenderOptions{})
}

func NewSenderWithOptions(stateDir string, options SenderOptions) (*Sender, error) {
	cache, err := NewCache(stateDir)
	if err != nil {
		return nil, err
	}
	if options.Discover == nil {
		options.Discover = func(ctx context.Context) (NIC, error) { return discoverNIC(ctx, false) }
	}
	if options.Observe == nil {
		options.Observe = observeTarget
	}
	if options.Transmit == nil {
		options.Transmit = sendPacket
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Sender{cache: cache, options: options}, nil
}

// Probe only reports Down after a matching LAN answers independently of the target.
func (s *Sender) Probe(ctx context.Context, grant Grant) (State, error) {
	if _, err := s.prepare(ctx, grant); err != nil {
		return Unknown, err
	}
	state, err := s.options.Observe(ctx, *grant.NIC)
	if ctx.Err() != nil {
		return Unknown, ctx.Err()
	}
	if err != nil {
		return Unknown, nil
	}
	return state, nil
}

// Send reserves the cooldown before transmission, including ambiguous send failures.
func (s *Sender) Send(ctx context.Context, grant Grant) (bool, error) {
	local, err := s.prepare(ctx, grant)
	if err != nil {
		return false, err
	}
	lock, err := lockFile(ctx, s.cache.path(grant.TargetID)+".lock")
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Close() }()
	current, err := s.cache.Get(grant.TargetID)
	if err != nil {
		return false, err
	}
	if err := validateCurrentPolicy(current, grant); err != nil {
		return false, err
	}
	if err := ValidateGrant(grant, time.Now()); err != nil {
		return false, err
	}
	reserved, err := s.reserve(grant.TargetID)
	if err != nil || !reserved {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.options.Transmit(ctx, *grant.NIC, local.Address); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Sender) prepare(ctx context.Context, grant Grant) (NIC, error) {
	if err := ctx.Err(); err != nil {
		return NIC{}, err
	}
	if err := s.cache.PutContext(ctx, grant); err != nil {
		return NIC{}, err
	}
	if !grant.Enabled {
		return NIC{}, ErrDisabled
	}
	local, err := s.options.Discover(ctx)
	if err != nil {
		return NIC{}, fmt.Errorf("%w: %v", ErrNoLAN, err)
	}
	if err := validateNIC(local); err != nil {
		return NIC{}, err
	}
	if local.Prefix != grant.NIC.Prefix || local.GatewayMAC != grant.NIC.GatewayMAC {
		return NIC{}, ErrNoLAN
	}
	return local, nil
}

func (s *Sender) reserve(targetID string) (bool, error) {
	path := s.cache.path(targetID) + ".cooldown"
	var until time.Time
	err := readJSON(path, &until)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	now := s.options.Now()
	if now.Before(until) {
		return false, nil
	}
	if err := writeJSON(path, now.Add(Cooldown)); err != nil {
		return false, err
	}
	return true, nil
}

func observeTarget(ctx context.Context, target NIC) (State, error) {
	reachable, err := ping(ctx, target.Address)
	if err != nil {
		return Unknown, err
	}
	if reachable {
		return Reachable, nil
	}
	route, err := defaultRoute(ctx)
	if err != nil {
		return Unknown, err
	}
	reachable, err = ping(ctx, route.gateway.String())
	if err != nil || !reachable {
		return Unknown, err
	}
	return Down, nil
}

func MagicPacket(mac string) ([]byte, error) {
	address, err := parseMAC(mac)
	if err != nil {
		return nil, err
	}
	packet := make([]byte, 6+16*6)
	for index := range 6 {
		packet[index] = 0xff
	}
	for index := range 16 {
		copy(packet[6+index*6:], address)
	}
	return packet, nil
}

func sendPacket(ctx context.Context, nic NIC, source string) error {
	packet, err := MagicPacket(nic.MAC)
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(nic.Prefix)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(source)})
	if err != nil {
		return fmt.Errorf("open wake broadcast socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fmt.Errorf("set wake send deadline: %w", err)
	}
	for _, port := range []int{7, 9} {
		if err := writePacket(conn, packet, &net.UDPAddr{IP: net.IP(broadcast(prefix).AsSlice()), Port: port}); err != nil {
			return err
		}
	}
	return nil
}

func writePacket(conn *net.UDPConn, packet []byte, address *net.UDPAddr) error {
	written, err := conn.WriteToUDP(packet, address)
	if err != nil {
		return fmt.Errorf("send wake packet to %s: %w", address, err)
	}
	if written != len(packet) {
		return errors.New("send wake packet: short UDP write")
	}
	return nil
}
