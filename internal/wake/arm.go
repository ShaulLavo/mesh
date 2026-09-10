package wake

import (
	"errors"
	"fmt"
	"net"
)

var (
	// ErrArmUnsupported reports hardware or a platform that cannot wake on a
	// magic packet. Nothing the caller does will make waking work.
	ErrArmUnsupported = errors.New("target interface does not support wake on magic packet")
	// ErrArmPrivilege reports that arming needs root. Callers re-run themselves
	// under sudo rather than asking the operator to run ethtool by hand.
	ErrArmPrivilege = errors.New("arming wake requires root")
)

// ArmState is what the NIC will actually do, not what policy asks of it.
type ArmState struct {
	Device string `json:"device"`
	// Supported is the driver's own claim that it can wake on a magic packet.
	Supported bool `json:"supported"`
	// Armed is whether it is set to do so right now.
	Armed bool `json:"armed"`
	// Persisted is whether it will still be armed after a reboot.
	Persisted bool `json:"persisted"`
	// Firmware is the ACPI wake source state for the NIC: "enabled",
	// "disabled", or empty when the platform reports none. Mesh cannot change
	// it. A disabled source means the board ignores the packet once it sleeps,
	// however well the driver is armed.
	Firmware string `json:"firmware,omitempty"`
}

// Ready reports whether a wake grant issued now would be honoured.
func (s ArmState) Ready() bool { return s.Supported && s.Armed }

// deviceForMAC resolves the interface the discovered NIC describes. Discovery
// records a MAC rather than a name because names are not stable across boots,
// so every privileged step has to resolve it again.
func deviceForMAC(mac string) (string, error) {
	address, err := parseMAC(mac)
	if err != nil {
		return "", fmt.Errorf("wake NIC MAC: %w", err)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, candidate := range interfaces {
		if candidate.HardwareAddr.String() == address.String() {
			return candidate.Name, nil
		}
	}
	return "", fmt.Errorf("no interface has MAC %s", address)
}
