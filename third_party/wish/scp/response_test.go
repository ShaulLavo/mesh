package scp

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	gossh "golang.org/x/crypto/ssh"
)

func TestCopyToClientWaitsForFinalAcknowledgement(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	session := setup(t, NewFSReadHandler(os.DirFS(root)), nil)
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("scp -f file.txt"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	acknowledge := func() {
		t.Helper()
		if _, err := input.Write(NULL); err != nil {
			t.Fatal(err)
		}
	}
	acknowledge()
	header, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(header, "T") {
		acknowledge()
		header, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(header, "C") || !strings.HasSuffix(header, " 8 file.txt\n") {
		t.Fatalf("file header = %q", header)
	}
	acknowledge()
	body := make([]byte, 9)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "contents\x00" {
		t.Fatalf("body = %q", body)
	}
	exited := make(chan error, 1)
	go func() { exited <- session.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("sender exited before the final client acknowledgement: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	acknowledge()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sender did not finish after the final acknowledgement")
	}
}

func setupAcknowledgedSource(tb testing.TB, handler CopyToClientHandler) *gossh.Session {
	tb.Helper()
	session := setup(tb, handler, nil)
	session.Stdin = bytes.NewReader(make([]byte, 128))
	return session
}

func TestFileAndDirectoryWritesWaitAtEveryRecord(t *testing.T) {
	file := &FileEntry{Name: "file", Filepath: "file", Mode: 0600, Size: 4,
		Reader: strings.NewReader("data"), Mtime: 20, Atime: 10}
	directory := &DirEntry{Name: "dir", Filepath: "dir", Mode: 0700, Mtime: 40,
		Atime: 30, Children: []Entry{file}}
	var output bytes.Buffer
	script := &responseScript{t: t, output: &output, records: []string{
		"T40 0 30 0\n", "D0700 0 dir\n", "T20 0 10 0\n", "C0600 4 file\n", "data\x00", "E\n",
	}}
	writer := &clientWriter{Session: &responseSession{writer: &output}, responses: bufio.NewReader(script), timeout: time.Second, connection: io.NopCloser(strings.NewReader(""))}
	if err := directory.Write(writer); err != nil {
		t.Fatal(err)
	}
	if script.offset != len(script.records) {
		t.Fatalf("acknowledged %d of %d records", script.offset, len(script.records))
	}
}

type responseScript struct {
	t       *testing.T
	output  *bytes.Buffer
	records []string
	offset  int
}

func (r *responseScript) Read(buffer []byte) (int, error) {
	r.t.Helper()
	if r.offset >= len(r.records) {
		r.t.Fatal("sender requested an extra acknowledgement")
	}
	if actual := r.output.String(); actual != r.records[r.offset] {
		r.t.Fatalf("output before acknowledgement %d = %q, want %q", r.offset, actual, r.records[r.offset])
	}
	r.output.Reset()
	r.offset++
	buffer[0] = 0
	return 1, nil
}

func TestClientRefusesTransfer(t *testing.T) {
	for _, response := range []string{"\x01disk full\n", "\x02cannot create file\n", "\x03", "", "\x01" + strings.Repeat("x", 5000)} {
		t.Run(fmtResponseName(response), func(t *testing.T) {
			writer := &clientWriter{responses: bufio.NewReader(strings.NewReader(response)), timeout: time.Second, connection: io.NopCloser(strings.NewReader(""))}
			if err := writer.awaitResponse(); err == nil {
				t.Fatal("accepted a missing, invalid, or refused acknowledgement")
			}
		})
	}
}

func fmtResponseName(response string) string {
	if len(response) > 30 {
		return "oversized refusal"
	}
	if response == "" {
		return "disconnect"
	}
	return response
}

func TestClientResponseTimeoutClosesTransportWithoutChannelHandshake(t *testing.T) {
	completed := make(chan error, 1)
	start := make(chan struct{})
	server := &ssh.Server{Handler: func(s ssh.Session) {
		<-start
		writer := &clientWriter{Session: s, responses: bufio.NewReader(s),
			connection: s.Context().Value(ssh.ContextKeyConn).(io.Closer), timeout: 20 * time.Millisecond}
		completed <- writer.awaitResponse()
	}}
	session, peer := newDroppingPeer(t, server)
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := session.Start("scp -f file"); err != nil {
		t.Fatal(err)
	}
	peer.discard.Store(true)
	close(start)
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("missing acknowledgement succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("transport remained open while the peer withheld acknowledgements and EOF")
	}
	if err := session.Wait(); err == nil {
		t.Fatal("timed-out transfer succeeded")
	}
}

func newDroppingPeer(t *testing.T, server *ssh.Server) (*gossh.Session, *droppingConn) {
	t.Helper()
	address := testsession.Listen(t, server)
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	peer := &droppingConn{Conn: connection}
	transport, channels, requests, err := gossh.NewClientConn(peer, address, &gossh.ClientConfig{
		User: "testuser", HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(transport, channels, requests)
	t.Cleanup(func() { _ = client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return session, peer
}

type droppingConn struct {
	net.Conn
	discard atomic.Bool
}

func (c *droppingConn) Write(buffer []byte) (int, error) {
	if c.discard.Load() {
		return len(buffer), nil
	}
	return c.Conn.Write(buffer)
}

type responseSession struct {
	ssh.Session
	writer io.Writer
}

func (s *responseSession) Write(buffer []byte) (int, error) { return s.writer.Write(buffer) }

func TestCopyToClientStopsAtInitialRefusal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	session := setup(t, NewFSReadHandler(os.DirFS(root)), nil)
	session.Stdin = strings.NewReader("\x02cannot create destination\n")
	output, err := session.Output("scp -f file.txt")
	if err == nil {
		t.Fatal("client refusal did not fail the transfer")
	}
	if len(output) != 0 {
		t.Fatalf("sent records after initial refusal: %q", output)
	}
}

func TestFileWriteStopsAtHeaderRefusal(t *testing.T) {
	var output bytes.Buffer
	file := &FileEntry{Name: "file", Filepath: "file", Mode: 0600, Size: 4, Reader: strings.NewReader("data")}
	writer := &clientWriter{Session: &responseSession{writer: &output}, responses: bufio.NewReader(strings.NewReader("\x01disk full\n")), timeout: time.Second, connection: io.NopCloser(strings.NewReader(""))}
	if err := file.Write(writer); err == nil {
		t.Fatal("client refusal did not fail the file transfer")
	}
	if actual := output.String(); actual != "C0600 4 file\n" {
		t.Fatalf("sent data beyond refused header: %q", actual)
	}
}
