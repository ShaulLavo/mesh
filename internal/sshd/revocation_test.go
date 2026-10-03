package sshd

import (
	"path/filepath"
	"testing"
	"time"

	"charm.land/wish/v2/testsession"
	"github.com/shaul/mesh/internal/identity"
	gossh "golang.org/x/crypto/ssh"
)

func TestDeviceRevocationClosesIdleSSHAndPreservesOtherDevice(t *testing.T) {
	directory := t.TempDir()
	first, firstKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, secondKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := identity.ApproveDevice(directory, id); err != nil {
			t.Fatal(err)
		}
	}
	server := mustServer(t, Config{HostKey: generatePrivateKey(t), AuthorizedKeys: filepath.Join(directory, "authorized_keys"), Addr: "127.0.0.1:2222"})
	address := testsession.Listen(t, server)
	client, err := gossh.Dial("tcp", address, clientConfig(t, firstKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // fixture cleanup
	closed := make(chan error, 1)
	go func() { closed <- client.Wait() }()
	if err := identity.RevokeDevice(directory, first.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked idle SSH connection remained open")
	}
	if replacement, err := gossh.Dial("tcp", address, clientConfig(t, firstKey, nil)); err == nil {
		_ = replacement.Close()
		t.Fatal("revoked SSH identity reconnected")
	}
	session, err := testsession.NewClientSession(t, address, clientConfig(t, secondKey, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run(""); err != nil {
		t.Fatal(err)
	}
}
