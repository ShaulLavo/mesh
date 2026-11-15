package transport

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
)

func TestAuthenticationInterruptionPreservesCauseAndRetryDecision(t *testing.T) {
	for _, cause := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, syscall.ECONNRESET, syscall.EPIPE} {
		err := authenticationFailure("handshake", cause)
		if !errors.Is(err, cause) || !errors.Is(err, ErrAuthentication) || permanentReconnectError(err) {
			t.Fatalf("interrupted authentication lost cause or became permanent: %v", err)
		}
	}
	refusal := errors.New("destination refused this device")
	err := authenticationFailure("peer acceptance", refusal)
	if !errors.Is(err, refusal) || !permanentReconnectError(err) {
		t.Fatalf("peer refusal became retryable: %v", err)
	}
}
