package release

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

const (
	downloadAttempts = 3
	// Leave room for RPC framing while a retry can wait through a 20-second DNS stall.
	metadataDownloadTimeout = 40 * time.Second
	metadataInitialTimeout  = 5 * time.Second
	assetDownloadTimeout    = 60 * time.Second
	downloadAttemptTimeout  = 30 * time.Second
	downloadBackoff         = 250 * time.Millisecond
)

func retryDownload(ctx context.Context, totalTimeout, initialTimeout time.Duration, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	unclassifiedRetried := false
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("release: download cancelled: %w", err)
		}
		timeout := downloadAttemptTimeout
		if attempt == 1 {
			timeout = initialTimeout
		}
		err := downloadAttempt(ctx, timeout, attempt, operation)
		if err == nil {
			return nil
		}
		policy := downloadRetryPolicy(err)
		if ctx.Err() != nil || attempt == downloadAttempts || policy == downloadTerminal || (policy == downloadUnclassified && unclassifiedRetried) {
			return err
		}
		if policy == downloadUnclassified {
			unclassifiedRetried = true
		}
		if err := waitDownloadRetry(ctx, downloadBackoff<<(attempt-1)); err != nil {
			return fmt.Errorf("release: download backoff after attempt %d: %w", attempt, err)
		}
	}
	return nil
}

func downloadAttempt(ctx context.Context, timeout time.Duration, attempt int, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	trace := &downloadTrace{stage: "connect"}
	err := operation(httptrace.WithClientTrace(ctx, trace.clientTrace()))
	if err == nil {
		return nil
	}
	var transport *downloadTransportError
	if errors.As(err, &transport) {
		transport.awaitingHTTP2Headers = trace.awaitingHTTP2Headers()
	}
	return fmt.Errorf("attempt %d/%d failed during %s: %w", attempt, downloadAttempts, trace.currentStage(), err)
}

func waitDownloadRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for download retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

type downloadStatusError struct {
	code int
	err  error
}

func (e *downloadStatusError) Error() string { return e.err.Error() }
func (e *downloadStatusError) Unwrap() error { return e.err }

type downloadRetry uint8

const (
	downloadTerminal downloadRetry = iota
	downloadTransient
	downloadUnclassified
)

type downloadTransportError struct {
	err                  error
	awaitingHTTP2Headers bool
}

func (e *downloadTransportError) Error() string { return e.err.Error() }
func (e *downloadTransportError) Unwrap() error { return e.err }

type downloadRedirectError struct{ err error }

func (e *downloadRedirectError) Error() string { return e.err.Error() }
func (e *downloadRedirectError) Unwrap() error { return e.err }

func downloadRetryPolicy(err error) downloadRetry {
	var redirect *downloadRedirectError
	if errors.As(err, &redirect) || errors.Is(err, context.Canceled) {
		return downloadTerminal
	}
	var read *downloadReadError
	if errors.As(err, &read) {
		return downloadTransient
	}
	var local *os.PathError
	if errors.As(err, &local) {
		return downloadTerminal
	}
	var status *downloadStatusError
	if errors.As(err, &status) {
		if status.code == http.StatusRequestTimeout || status.code == http.StatusTooManyRequests || status.code >= 500 {
			return downloadTransient
		}
		return downloadTerminal
	}
	return downloadTransportPolicy(err)
}

func downloadTransportPolicy(err error) downloadRetry {
	if permanentDownloadTLS(err) {
		return downloadTerminal
	}
	var stream http2.StreamError
	if errors.As(err, &stream) {
		return downloadStreamPolicy(stream.Code)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout || dns.IsTemporary {
			return downloadTransient
		}
		return downloadTerminal
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return downloadTransient
	}
	var connection *net.OpError
	if errors.As(err, &connection) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return downloadTransient
	}
	var transport *downloadTransportError
	if errors.As(err, &transport) && transport.awaitingHTTP2Headers {
		return downloadUnclassified
	}
	return downloadTerminal
}

func downloadStreamPolicy(code http2.ErrCode) downloadRetry {
	if code == http2.ErrCodeInternal || code == http2.ErrCodeRefusedStream || code == http2.ErrCodeCancel || code == http2.ErrCodeEnhanceYourCalm {
		return downloadTransient
	}
	return downloadTerminal
}

func permanentDownloadTLS(err error) bool {
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	return errors.As(err, &certificate) || errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &roots)
}

// Transport callbacks may run concurrently, including after an attempt has timed out.
type downloadTrace struct {
	mu              sync.Mutex
	stage           string
	http2           bool
	requestWritten  bool
	responseStarted bool
}

func (t *downloadTrace) setStage(stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stage = stage
}

func (t *downloadTrace) currentStage() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stage
}

func (t *downloadTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.stage = "connect"
			t.http2, t.requestWritten, t.responseStarted = false, false, false
		},
		DNSStart: func(httptrace.DNSStartInfo) { t.setStage("DNS") },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err == nil {
				t.setStage("connect")
			}
		},
		ConnectStart:      func(string, string) { t.setStage("connect") },
		TLSHandshakeStart: func() { t.setStage("TLS handshake") },
		GotConn: func(info httptrace.GotConnInfo) {
			connection, ok := info.Conn.(interface{ ConnectionState() tls.ConnectionState })
			t.mu.Lock()
			defer t.mu.Unlock()
			t.stage = "request write"
			t.http2 = ok && connection.ConnectionState().NegotiatedProtocol == "h2"
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.requestWritten = info.Err == nil
			if t.requestWritten {
				t.stage = "read"
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.responseStarted = true
		},
	}
}

// Opaque HTTP/2 failures get one retry only after a complete GET and before headers.
func (t *downloadTrace) awaitingHTTP2Headers() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.http2 && t.requestWritten && !t.responseStarted
}
