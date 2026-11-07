package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"io"
	"os"
	"time"
)

func run() error {
	if len(os.Args) == 5 && os.Args[3] == "scoped-denial" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return scopedDenial(ctx, os.Args[1], os.Args[2], os.Args[4])
	}
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

type scopedProbe struct {
	Kind      protocol.Kind `json:"kind"`
	SessionID string        `json:"sessionId"`
	Payload   []byte        `json:"payload"`
}

func scopedDenial(ctx context.Context, endpoint, pin, directory string) error {
	_, key, err := identity.LoadCredential(directory)
	if err != nil {
		return fmt.Errorf("fixture scoped credential: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil {
		return fmt.Errorf("fixture scoped probes: %w", err)
	}
	if len(data) > 64<<10 {
		return fmt.Errorf("fixture scoped probes exceed limit")
	}
	var probes []scopedProbe
	if err := json.Unmarshal(data, &probes); err != nil {
		return fmt.Errorf("fixture decode scoped probes: %w", err)
	}
	if len(probes) == 0 || len(probes) > 64 {
		return fmt.Errorf("fixture scoped probe count is invalid")
	}
	auth := &transport.Authentication{Key: key, ExpectedIdentity: pin, Admission: transport.ScopedAdmission}
	for _, probe := range probes {
		if err := sendScopedProbe(ctx, endpoint, auth, probe); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(os.Stdout, "PASS: %d scoped frames denied before reply\n", len(probes))
	if err != nil {
		return fmt.Errorf("fixture scoped output: %w", err)
	}
	return nil
}
func sendScopedProbe(ctx context.Context, endpoint string, auth *transport.Authentication, probe scopedProbe) error {
	conn, err := transport.DialOnce(ctx, endpoint, transport.DialOptions{Auth: auth})
	if err != nil {
		return fmt.Errorf("fixture scoped TLS admission: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	frame := protocol.Frame{Kind: probe.Kind, Payload: probe.Payload}
	if probe.SessionID != "" {
		id, err := protocol.NewSessionID(probe.SessionID)
		if err != nil {
			return fmt.Errorf("fixture scoped session: %w", err)
		}
		frame.Session = id
	}
	if err := conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("fixture scoped probe write: %w", err)
	}
	if reply, err := conn.ReadFrame(); err == nil {
		return fmt.Errorf("scoped denial disclosed a frame of %d bytes", len(reply.Payload))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fixture scoped stream did not retire before deadline: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
