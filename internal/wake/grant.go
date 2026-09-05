// Package wake records target-owned wake permission and sends local broadcast packets.
package wake

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"time"
)

const GrantLifetime = 30 * 24 * time.Hour

var (
	ErrDisabled       = errors.New("target has not allowed wake")
	ErrNoLAN          = errors.New("no matching wired LAN is available")
	ErrUnsupportedNIC = errors.New("target interface does not support wired wake")
	ErrExpired        = errors.New("wake permission has expired; reconnect to the target to refresh it")
	ErrStaleGrant     = errors.New("wake permission is older than the cached target policy")
)

type NIC struct {
	MAC        string `json:"mac"`
	Address    string `json:"address"`
	Prefix     string `json:"prefix"`
	GatewayMAC string `json:"gatewayMac"`
}

type Grant struct {
	TargetID  string    `json:"targetId"`
	Enabled   bool      `json:"enabled"`
	Revision  uint64    `json:"revision"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	NIC       *NIC      `json:"nic,omitempty"`
	Signature []byte    `json:"signature"`
}

func ValidateGrant(grant Grant, now time.Time) error {
	if err := validateSignedGrant(grant); err != nil {
		return err
	}
	if !now.Before(grant.ExpiresAt) {
		return ErrExpired
	}
	if grant.IssuedAt.After(now.Add(5 * time.Minute)) {
		return errors.New("wake permission was issued in the future")
	}
	return nil
}

func validateSignedGrant(grant Grant) error {
	key, err := targetKey(grant.TargetID)
	if err != nil {
		return err
	}
	if grant.Revision == 0 || grant.Revision > math.MaxInt64 {
		return errors.New("wake permission revision is invalid")
	}
	if grant.IssuedAt.Year() < 1970 || grant.ExpiresAt.Year() > 9999 || !grant.ExpiresAt.After(grant.IssuedAt) || grant.ExpiresAt.Sub(grant.IssuedAt) > GrantLifetime {
		return errors.New("wake permission validity interval is invalid")
	}
	if grant.Enabled != (grant.NIC != nil) {
		return errors.New("wake permission enablement and NIC disagree")
	}
	if grant.NIC != nil {
		if err := validateNIC(*grant.NIC); err != nil {
			return err
		}
	}
	if !ed25519.Verify(key, grantTranscript(grant), grant.Signature) {
		return errors.New("wake permission signature is invalid")
	}
	return nil
}

func targetKey(id string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != id {
		return nil, errors.New("wake target ID is not an Ed25519 identity")
	}
	return ed25519.PublicKey(key), nil
}

func validateNIC(nic NIC) error {
	if _, err := parseMAC(nic.MAC); err != nil {
		return fmt.Errorf("wake NIC MAC: %w", err)
	}
	if _, err := parseMAC(nic.GatewayMAC); err != nil {
		return fmt.Errorf("wake gateway MAC: %w", err)
	}
	address, err := netip.ParseAddr(nic.Address)
	if err != nil || !usableIPv4(address) {
		return errors.New("wake NIC address is not a unicast IPv4 address")
	}
	prefix, err := netip.ParsePrefix(nic.Prefix)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 8 || prefix.Bits() > 30 || prefix != prefix.Masked() {
		return errors.New("wake NIC prefix must be a masked IPv4 /8 through /30 subnet")
	}
	if !prefix.Contains(address) || address == prefix.Addr() || address == broadcast(prefix) {
		return errors.New("wake NIC address is outside usable subnet addresses")
	}
	return nil
}

func usableIPv4(address netip.Addr) bool {
	return address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
}

func parseMAC(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() != value {
		return nil, errors.New("expected canonical unicast Ethernet MAC")
	}
	if mac[0]|mac[1]|mac[2]|mac[3]|mac[4]|mac[5] == 0 {
		return nil, errors.New("Ethernet MAC is zero")
	}
	return mac, nil
}

func grantTranscript(grant Grant) []byte {
	grant.Signature = nil
	grant.IssuedAt = grant.IssuedAt.UTC()
	grant.ExpiresAt = grant.ExpiresAt.UTC()
	encoded, _ := json.Marshal(grant)
	return append([]byte("mesh-wake-grant-v1\x00"), encoded...)
}

func samePolicy(a, b Grant) bool {
	if a.TargetID != b.TargetID || a.Enabled != b.Enabled {
		return false
	}
	if a.NIC == nil || b.NIC == nil {
		return a.NIC == nil && b.NIC == nil
	}
	return *a.NIC == *b.NIC
}

func broadcast(prefix netip.Prefix) netip.Addr {
	address := prefix.Masked().Addr().As4()
	bits := 32 - prefix.Bits()
	for index := 3; index >= 0 && bits > 0; index-- {
		count := min(bits, 8)
		address[index] |= byte((1 << count) - 1)
		bits -= count
	}
	return netip.AddrFrom4(address)
}
