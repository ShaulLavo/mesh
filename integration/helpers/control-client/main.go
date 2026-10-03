package main

import (
	"context"
	"fmt"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"io"
	"os"
	"time"
)

func run() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("usage: control-client ENDPOINT PIN JSON")
	}
	auth, err := transport.LocalAuthentication(os.Args[2])
	if err != nil {
		return fmt.Errorf("fixture identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if os.Args[3] == "grant-lifetime" {
		return grantLifetime(ctx, auth)
	}
	conn, err := transport.DialOnce(ctx, os.Args[1], transport.DialOptions{Auth: auth})
	if err != nil {
		return fmt.Errorf("fixture authenticated dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck // fixture cleanup
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: []byte(os.Args[3])}); err != nil {
		return fmt.Errorf("fixture control write: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("fixture control read: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return fmt.Errorf("fixture response kind=%d", frame.Kind)
	}
	if _, err := fmt.Fprintln(os.Stdout, string(frame.Payload)); err != nil {
		return fmt.Errorf("fixture output: %w", err)
	}
	return nil
}

func grantLifetime(ctx context.Context, auth *transport.Authentication) error {
	var attached []transport.Conn
	defer func() {
		for _, conn := range attached {
			_ = conn.Close()
		}
	}()
	for range 2 {
		conn, err := transport.DialOnce(ctx, os.Args[1], transport.DialOptions{Auth: auth})
		if err != nil {
			return fmt.Errorf("fixture old grant dial: %w", err)
		}
		context.AfterFunc(ctx, func() { _ = conn.Close() })
		attached = append(attached, conn)
	}
	if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
		return fmt.Errorf("fixture ready: %w", err)
	}
	var proceed [1]byte
	if _, err := io.ReadFull(os.Stdin, proceed[:]); err != nil {
		return fmt.Errorf("fixture await reapproval: %w", err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeList, RequestID: "retired-observer"}).Encode()
	if err != nil {
		return fmt.Errorf("fixture list control: %w", err)
	}
	_ = attached[0].WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	for _, conn := range attached {
		if _, err := conn.ReadFrame(); err == nil {
			return fmt.Errorf("reapproval healed an old active or passive socket")
		}
	}
	if err := freshGrantList(ctx, auth, payload); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, "PASS: old active/passive sockets retired; fresh grant connected"); err != nil {
		return fmt.Errorf("fixture success output: %w", err)
	}
	return nil
}

func freshGrantList(ctx context.Context, auth *transport.Authentication, payload []byte) error {
	fresh, err := transport.DialOnce(ctx, os.Args[1], transport.DialOptions{Auth: auth})
	if err != nil {
		return fmt.Errorf("fixture fresh grant dial: %w", err)
	}
	defer fresh.Close() //nolint:errcheck // fixture cleanup
	if err := fresh.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("fixture fresh list: %w", err)
	}
	frame, err := fresh.ReadFrame()
	if err != nil {
		return fmt.Errorf("fixture fresh response: %w", err)
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if err != nil || control.Type != protocol.TypeListed {
		return fmt.Errorf("fixture fresh list response type=%q", control.Type)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
