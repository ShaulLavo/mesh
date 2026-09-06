package scp

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"charm.land/ssh"
)

const clientResponseTimeout = 30 * time.Second

type clientWriter struct {
	ssh.Session
	responses  *bufio.Reader
	connection io.Closer
	timeout    time.Duration
}

func (w *clientWriter) awaitResponse() error {
	// Closing a channel does not release its Read until the peer acknowledges
	// the close. Only the transport can bound a wait against a silent peer.
	timer := time.AfterFunc(w.timeout, func() { _ = w.connection.Close() })
	defer timer.Stop()
	code, err := w.responses.ReadByte()
	if err != nil {
		return fmt.Errorf("read SCP client acknowledgement: %w", err)
	}
	if code == 0 {
		return nil
	}
	if code != 1 && code != 2 {
		return fmt.Errorf("invalid SCP client acknowledgement: %d", code)
	}
	message, err := w.responses.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("read SCP client refusal: %w", err)
	}
	return fmt.Errorf("SCP client refused transfer: %s", message[:len(message)-1])
}

func writeRecord(w io.Writer, format string, arguments ...any) error {
	if _, err := fmt.Fprintf(w, format, arguments...); err != nil {
		return err
	}
	return awaitResponse(w)
}

func awaitResponse(w io.Writer) error {
	if client, ok := w.(*clientWriter); ok {
		return client.awaitResponse()
	}
	return nil
}
