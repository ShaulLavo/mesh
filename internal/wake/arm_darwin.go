package wake

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
)

// macOS stores wake on magic packet in the power management settings, so
// arming it is already durable and Persisted always tracks Armed.
func Inspect(ctx context.Context, mac string) (ArmState, error) {
	device, err := deviceForMAC(mac)
	if err != nil {
		return ArmState{}, err
	}
	state := ArmState{Device: device}
	contents, err := runCommand(ctx, "/usr/bin/pmset", "-g")
	if err != nil {
		return state, fmt.Errorf("read power management settings: %w", err)
	}
	value, found := pmsetValue(contents, "womp")
	if !found {
		return state, nil
	}
	state.Supported = true
	state.Armed = value != "0"
	state.Persisted = state.Armed
	return state, nil
}

func Arm(ctx context.Context, mac string) (ArmState, error) {
	state, err := Inspect(ctx, mac)
	if err != nil {
		return state, err
	}
	if !state.Supported {
		return state, fmt.Errorf("%w: no wake on magic packet setting for %s", ErrArmUnsupported, state.Device)
	}
	if !state.Armed {
		if _, err := runCommand(ctx, "/usr/bin/pmset", "-a", "womp", "1"); err != nil {
			return state, fmt.Errorf("enable wake on magic packet: %w", err)
		}
	}
	verified, err := Inspect(ctx, mac)
	if err != nil {
		return verified, err
	}
	if !verified.Ready() {
		return verified, fmt.Errorf("%w: %s did not keep wake on magic packet", ErrArmUnsupported, verified.Device)
	}
	return verified, nil
}

func Disarm(ctx context.Context, mac string) (ArmState, error) {
	state, err := Inspect(ctx, mac)
	if err != nil || !state.Armed {
		return state, err
	}
	if _, err := runCommand(ctx, "/usr/bin/pmset", "-a", "womp", "0"); err != nil {
		return state, fmt.Errorf("disable wake on magic packet: %w", err)
	}
	return Inspect(ctx, mac)
}

func pmsetValue(contents []byte, key string) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == key {
			return fields[1], true
		}
	}
	return "", false
}
