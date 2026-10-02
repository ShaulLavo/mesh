package release

import (
	"context"
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
		if ctx.Err() != nil || attempt == downloadAttempts || !retryableDownload(err) {
			return err
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

func retryableDownload(err error) bool {
	var read *downloadReadError
	if errors.As(err, &read) {
		return true
	}
	var local *os.PathError
	if errors.As(err, &local) {
		return false
	}
	var status *downloadStatusError
	if errors.As(err, &status) {
		return status.code == http.StatusRequestTimeout || status.code == http.StatusTooManyRequests || status.code >= 500
	}
	return retryableDownloadTransport(err)
}

func retryableDownloadTransport(err error) bool {
	var stream http2.StreamError
	if errors.As(err, &stream) {
		return stream.Code == http2.ErrCodeInternal || stream.Code == http2.ErrCodeRefusedStream || stream.Code == http2.ErrCodeCancel || stream.Code == http2.ErrCodeEnhanceYourCalm
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return true
	}
	var connection *net.OpError
	return errors.As(err, &connection) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Transport callbacks may run concurrently, including after an attempt has timed out.
type downloadTrace struct {
	mu    sync.Mutex
	stage string
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
		GetConn:  func(string) { t.setStage("connect") },
		DNSStart: func(httptrace.DNSStartInfo) { t.setStage("DNS") },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err == nil {
				t.setStage("connect")
			}
		},
		ConnectStart:      func(string, string) { t.setStage("connect") },
		TLSHandshakeStart: func() { t.setStage("TLS handshake") },
		GotConn:           func(httptrace.GotConnInfo) { t.setStage("request write") },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.setStage("read")
			}
		},
	}
}
