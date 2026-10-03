package main

import (
	"context"
	"fmt"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
