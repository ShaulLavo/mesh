package sshd

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/charmbracelet/wish/testsession"
)

func TestSSHRateLimitSharesBudgetAcrossSourcePorts(t *testing.T) {
	limiter := &sshRateLimiter{entries: make(map[string]*list.Element)}
	ip := net.ParseIP("192.0.2.10")
	for port := range 20 {
		if !limiter.allow(&net.TCPAddr{IP: ip, Port: 1000 + port}) {
			t.Fatalf("initial burst denied at request %d", port)
		}
	}
	if limiter.allow(&net.TCPAddr{IP: ip, Port: 9000}) {
		t.Fatal("changing source port bypassed the per-IP burst limit")
	}
	if !limiter.allow(&net.TCPAddr{IP: net.ParseIP("192.0.2.11"), Port: 9000}) {
		t.Fatal("one source consumed another source's budget")
	}
}

func TestSSHRateLimitKeepsBoundedRecentSources(t *testing.T) {
	limiter := &sshRateLimiter{entries: make(map[string]*list.Element)}
	for index := range maximumSSHRateEntries + 10 {
		address := &net.TCPAddr{IP: net.ParseIP(fmt.Sprintf("198.18.%d.%d", index/256, index%256)), Port: 1000}
		if !limiter.allow(address) {
			t.Fatalf("new source %s had no initial budget", address)
		}
	}
	if len(limiter.entries) != maximumSSHRateEntries || limiter.order.Len() != maximumSSHRateEntries {
		t.Fatalf("rate limiter retained %d sources and %d order entries", len(limiter.entries), limiter.order.Len())
	}
	if limiter.entries["198.18.0.0"] != nil || limiter.entries["198.18.4.9"] == nil {
		t.Fatal("rate limiter did not expire the oldest source")
	}
}

func TestSSHSessionPanicDoesNotStopServer(t *testing.T) {
	server, config := sessionTestServer(t, func(context.Context, Session) (int, error) {
		panic("test session failure")
	})
	address := testsession.Listen(t, server)
	failed, err := testsession.NewClientSession(t, address, config)
	if err != nil {
		t.Fatal(err)
	}
	keepInputOpen(t, failed)
	if err := failed.RequestPty("xterm", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	if err := failed.Run("7K3D"); err == nil {
		t.Fatal("panicking session returned success")
	}
	healthy, err := testsession.NewClientSession(t, address, config)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	healthy.Stdout = &output
	if err := healthy.Run(helloCommand); err != nil || output.String() != helloMessage {
		t.Fatalf("server after panic returned %q, %v", output.String(), err)
	}
}
