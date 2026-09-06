package sshfs_test

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/sshfs"
)

type fixture struct {
	client   *sftp.Client
	registry *serve.Registry
	root     string
	outside  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "files")
	static := filepath.Join(parent, "static")
	for _, directory := range []string{filepath.Join(root, "nested"), static} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "nested", "value.txt"), "download bytes\x00\xff\n")
	writeFile(t, filepath.Join(static, "index.html"), "<h1>static</h1>")
	outside := filepath.Join(parent, "secret.txt")
	writeFile(t, outside, "outside secret")
	registry, err := serve.NewRegistry([]serve.Service{
		{Name: "files", Kind: serve.Files, Target: root},
		{Name: "site", Kind: serve.Static, Target: static},
		{Name: "proxy", Kind: serve.Proxy, Target: "12345"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{client: newClient(t, registry), registry: registry, root: root, outside: outside}
}

func newClient(t *testing.T, registry *serve.Registry) *sftp.Client {
	t.Helper()
	client, err := sftp.NewClient(newSSHClient(t, registry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newSSHClient(t *testing.T, registry *serve.Registry) *gossh.Client {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := sshfs.New(registry)
	server := &charmssh.Server{
		HostSigners:       []charmssh.Signer{signer},
		SubsystemHandlers: map[string]charmssh.SubsystemHandler{"sftp": filesystem.Subsystem},
		Handler: filesystem.Middleware(func(session charmssh.Session) {
			_ = session.Exit(127)
		}),
	}
	address := testsession.Listen(t, server)
	connection, err := gossh.Dial("tcp", address, &gossh.ClientConfig{
		User: "mesh", Timeout: 3 * time.Second,
		HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestSFTPListsOnlyDeclaredFilesAndStaticServices(t *testing.T) {
	f := newFixture(t)
	assertNames(t, f.client, "/", "files", "site")
	assertNames(t, f.client, ".", "files", "site")
	assertNames(t, f.client, "/files", "nested")
	assertNames(t, f.client, "/files/nested", "value.txt")
	assertRead(t, f.client, "/files/nested/value.txt", "download bytes\x00\xff\n")
	assertRead(t, f.client, "site/index.html", "<h1>static</h1>")
	for _, path := range []string{"/", "/files", "/files/nested"} {
		info, err := f.client.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("Stat(%q) = %v, %v; want read-only directory", path, info, err)
		}
	}
	canonical, err := f.client.RealPath(".")
	if err != nil || canonical != "/" {
		t.Fatalf("RealPath(.) = %q, %v", canonical, err)
	}
	file, err := f.client.Open("/files/nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	info, err := file.Stat()
	if err != nil || info.Size() != int64(len("download bytes\x00\xff\n")) {
		t.Fatalf("Fstat() = %v, %v", info, err)
	}
	_, err = f.client.Stat("/proxy")
	assertPermission(t, err)
}

func TestSFTPRejectsTraversalForEveryPathOperation(t *testing.T) {
	f := newFixture(t)
	attacks := []string{
		"/etc/passwd", f.outside, "..", "/..", "/files/..",
		"/files/../site/index.html", "/files/nested/../nested/value.txt",
		"/files/nested/../../secret.txt", "/files/value.txt\x00/secret",
		"/files/..\\secret.txt",
	}
	for _, attack := range attacks {
		t.Run(attack, func(t *testing.T) { rejectAllPathOperations(t, f.client, attack) })
	}
	assertRead(t, f.client, "/files/nested/value.txt", "download bytes\x00\xff\n")
	assertLocalContents(t, f.outside, "outside secret")
}

func TestSFTPDoesNotDecodeHTTPEscapes(t *testing.T) {
	f := newFixture(t)
	paths := []string{
		"/files/%2e%2e/secret.txt", "/files/%2E%2E/secret.txt",
		"/files/%252e%252e/secret.txt", "/files/%25252e%25252e/secret.txt",
		"/files/%2e%2e%2fsecret.txt", "/files/%252e%252e%252fsecret.txt",
		"/files/nested%2f..%2f..%2fsecret.txt", "/files/value.txt%00/secret",
		"/files/value.txt%2500/secret",
	}
	for _, name := range paths {
		assertMissingLiteralPath(t, f.client, name)
	}
	assertLocalContents(t, f.outside, "outside secret")
}

func assertMissingLiteralPath(t *testing.T, client *sftp.Client, name string) {
	t.Helper()
	for operation, run := range readOperations(client) {
		if err := run(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s(%q) = %v, want missing literal filename", operation, name, err)
		}
	}
	for operation, run := range writeOperations(client) {
		t.Run(operation+name, func(t *testing.T) { assertPermission(t, run(name)) })
	}
}

func rejectAllPathOperations(t *testing.T, client *sftp.Client, path string) {
	t.Helper()
	operations := readOperations(client)
	for name, operation := range writeOperations(client) {
		operations[name] = operation
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) { assertPermission(t, operation(path)) })
	}
}

func readOperations(client *sftp.Client) map[string]func(string) error {
	return map[string]func(string) error{
		"open": func(path string) error {
			file, err := client.Open(path)
			if file != nil {
				_ = file.Close()
			}
			return err
		},
		"readdir":  func(path string) error { _, err := client.ReadDir(path); return err },
		"stat":     func(path string) error { _, err := client.Stat(path); return err },
		"lstat":    func(path string) error { _, err := client.Lstat(path); return err },
		"realpath": func(path string) error { _, err := client.RealPath(path); return err },
		"readlink": func(path string) error { _, err := client.ReadLink(path); return err },
	}
}

func writeOperations(client *sftp.Client) map[string]func(string) error {
	const existing = "/files/nested/value.txt"
	return map[string]func(string) error{
		"create": func(path string) error {
			file, err := client.Create(path)
			if file != nil {
				_ = file.Close()
			}
			return err
		},
		"chmod":              func(path string) error { return client.Chmod(path, 0o777) },
		"chown":              func(path string) error { return client.Chown(path, os.Getuid(), os.Getgid()) },
		"chtimes":            func(path string) error { return client.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)) },
		"truncate":           func(path string) error { return client.Truncate(path, 0) },
		"mkdir":              func(path string) error { return client.Mkdir(path) },
		"remove":             func(path string) error { return client.Remove(path) },
		"rmdir":              func(path string) error { return client.RemoveDirectory(path) },
		"rename source":      func(path string) error { return client.Rename(path, "/files/renamed") },
		"rename target":      func(path string) error { return client.Rename(existing, path) },
		"posixrename source": func(path string) error { return client.PosixRename(path, "/files/renamed") },
		"posixrename target": func(path string) error { return client.PosixRename(existing, path) },
		"symlink source":     func(path string) error { return client.Symlink(path, "/files/new-link") },
		"symlink target":     func(path string) error { return client.Symlink(existing, path) },
		"hardlink source":    func(path string) error { return client.Link(path, "/files/new-link") },
		"hardlink target":    func(path string) error { return client.Link(existing, path) },
	}
}

func TestSFTPConfinesSymlinkMetadataAndNewSymlinks(t *testing.T) {
	f := newFixture(t)
	links := map[string]string{
		"relative-link":     "nested/value.txt",
		"absolute-link":     filepath.Join(f.root, "nested", "value.txt"),
		"directory-link":    "nested",
		"outside-link":      f.outside,
		"outside-directory": filepath.Dir(f.outside),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(f.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"relative-link", "absolute-link"} {
		assertSafeLink(t, f.client, "/files/"+name)
	}
	assertRead(t, f.client, "/files/directory-link/value.txt", "download bytes\x00\xff\n")
	for _, path := range []string{"/files/outside-link", "/files/outside-directory", "/files/outside-directory/secret.txt"} {
		t.Run(path, func(t *testing.T) { rejectAllPathOperations(t, f.client, path) })
	}
	if err := os.Remove(filepath.Join(f.root, "relative-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, filepath.Join(f.root, "relative-link")); err != nil {
		t.Fatal(err)
	}
	rejectAllPathOperations(t, f.client, "/files/relative-link")
	assertRead(t, f.client, "/files/nested/value.txt", "download bytes\x00\xff\n")
}

func assertSafeLink(t *testing.T, client *sftp.Client, path string) {
	t.Helper()
	assertRead(t, client, path, "download bytes\x00\xff\n")
	info, err := client.Lstat(path)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat(%q) = %v, %v; want symlink", path, info, err)
	}
	info, err = client.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("Stat(%q) = %v, %v; want regular file", path, info, err)
	}
	for _, operation := range []func(string) (string, error){client.RealPath, client.ReadLink} {
		canonical, err := operation(path)
		if err != nil || canonical != "/files/nested/value.txt" {
			t.Fatalf("canonical path for %q = %q, %v", path, canonical, err)
		}
	}
}

func TestSFTPRefusesEveryWriteWithoutChangingDisk(t *testing.T) {
	f := newFixture(t)
	for name, operation := range writeOperations(f.client) {
		t.Run(name, func(t *testing.T) { assertPermission(t, operation("/files/nested/value.txt")) })
	}
	for _, flags := range []int{os.O_WRONLY, os.O_RDWR, os.O_APPEND, os.O_CREATE, os.O_TRUNC, os.O_EXCL, os.O_WRONLY | os.O_TRUNC, os.O_RDWR | os.O_CREATE} {
		assertOpenFlagsDenied(t, f.client, flags)
	}
	file, err := f.client.Open("/files/nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	assertPermission(t, file.Chmod(0o777))
	assertPermission(t, file.Chown(os.Getuid(), os.Getgid()))
	assertPermission(t, file.Truncate(0))
	if _, err := file.WriteAt([]byte("overwrite"), 0); err == nil {
		t.Fatal("writing to a read-only handle succeeded")
	}
	assertLocalContents(t, filepath.Join(f.root, "nested", "value.txt"), "download bytes\x00\xff\n")
	assertLocalContents(t, f.outside, "outside secret")
	assertNames(t, f.client, "/files", "nested")
	info, err := os.Stat(filepath.Join(f.root, "nested", "value.txt"))
	if err != nil || info.Mode().Perm() != 0o600 || info.ModTime().Unix() == 1 {
		t.Fatalf("local file metadata changed: %v, %v", info, err)
	}
}

func TestSFTPReadErrorsDoNotExposeHostPaths(t *testing.T) {
	f := newFixture(t)
	file, err := f.client.Open("/files/nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	_, err = file.ReadAt(make([]byte, 1), -1)
	assertPermission(t, err)
	assertRead(t, f.client, "/files/nested/value.txt", "download bytes\x00\xff\n")
}

func assertOpenFlagsDenied(t *testing.T, client *sftp.Client, flags int) {
	t.Helper()
	file, err := client.OpenFile("/files/nested/value.txt", flags)
	if file != nil {
		_ = file.Close()
	}
	assertPermission(t, err)
}

func TestSFTPDeletedRootReportsErrorAndServerSurvives(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	for name, operation := range readOperations(f.client) {
		t.Run(name, func(t *testing.T) {
			if err := operation("/files"); err == nil {
				t.Fatal("deleted root remained accessible")
			}
		})
	}
	assertRead(t, f.client, "/site/index.html", "<h1>static</h1>")
	assertRead(t, newClient(t, f.registry), "/site/index.html", "<h1>static</h1>")
}

func TestSFTPLiveRegistryChangesAndNestedServiceNames(t *testing.T) {
	f := newFixture(t)
	if err := f.registry.Replace([]serve.Service{{Name: "team/docs", Kind: serve.Files, Target: f.root}}); err != nil {
		t.Fatal(err)
	}
	assertNames(t, f.client, "/", "team")
	assertNames(t, f.client, "/team", "docs")
	assertRead(t, f.client, "/team/docs/nested/value.txt", "download bytes\x00\xff\n")
	_, err := f.client.Stat("/files/nested/value.txt")
	assertPermission(t, err)
	_, err = f.client.Stat("/team/docs/../../files/nested/value.txt")
	assertPermission(t, err)
	if err := f.registry.Replace(nil); err != nil {
		t.Fatal(err)
	}
	assertNames(t, f.client, "/")
	_, err = f.client.Stat("/team/docs/nested/value.txt")
	assertPermission(t, err)
}

func TestSCPLegacySourceTransfersOnlyFileContents(t *testing.T) {
	f := newFixture(t)
	connection := newSSHClient(t, f.registry)
	session, reader, input := startSCPDownload(t, connection, "scp -f /files/nested/value.txt")
	assertSCPFileRecord(t, reader, input, "value.txt", "download bytes\x00\xff\n")
	if err := session.Wait(); err != nil {
		t.Fatalf("SCP transfer exit: %v", err)
	}
}

func TestSCPDownloadsSafeDirectorySymlink(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink("nested", filepath.Join(f.root, "directory-link")); err != nil {
		t.Fatal(err)
	}
	connection := newSSHClient(t, f.registry)
	session, reader, input := startSCPDownload(t, connection, "scp -r -f /files/directory-link")
	assertSCPHeader(t, reader, input, "D0500 0 directory-link\n")
	assertSCPFileRecord(t, reader, input, "value.txt", "download bytes\x00\xff\n")
	assertSCPHeader(t, reader, input, "E\n")
	if err := session.Wait(); err != nil {
		t.Fatalf("recursive symlink download: %v", err)
	}
}

func startSCPDownload(t *testing.T, connection *gossh.Client, command string) (*gossh.Session, *bufio.Reader, io.Writer) {
	t.Helper()
	session, err := connection.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	output, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(command); err != nil {
		t.Fatal(err)
	}
	assertSCPAcknowledge(t, input)
	return session, bufio.NewReader(output), input
}

func TestSCPRefusesDirectorySymlinkCycleAndKeepsConnectionAlive(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(".", filepath.Join(f.root, "cycle")); err != nil {
		t.Fatal(err)
	}
	connection := newSSHClient(t, f.registry)
	assertSCPRefused(t, connection, "scp -r -f /files", f.root)
	client, err := sftp.NewClient(connection)
	if err != nil {
		t.Fatalf("new SFTP session after SCP cycle: %v", err)
	}
	defer client.Close() //nolint:errcheck // test cleanup
	assertRead(t, client, "/files/nested/value.txt", "download bytes\x00\xff\n")
}

func assertSCPFileRecord(t *testing.T, reader *bufio.Reader, input io.Writer, wantName, want string) {
	t.Helper()
	header := readSCPHeader(t, reader, input)
	var mode uint32
	var size int
	var name string
	if _, err := fmt.Sscanf(header, "C%o %d %s", &mode, &size, &name); err != nil {
		t.Fatalf("invalid SCP header %q: %v", header, err)
	}
	if mode&0o222 != 0 || size != len(want) || name != wantName {
		t.Fatalf("SCP header = %q", header)
	}
	assertSCPAcknowledge(t, input)
	contents := make([]byte, len(want)+1)
	if _, err := io.ReadFull(reader, contents); err != nil {
		t.Fatal(err)
	}
	if string(contents) != want+"\x00" {
		t.Fatalf("SCP contents = %q", contents)
	}
	assertSCPAcknowledge(t, input)
}

func TestSCPRejectsUploadsTraversalAndUnsupportedCommands(t *testing.T) {
	f := newFixture(t)
	connection := newSSHClient(t, f.registry)
	commands := []string{
		"scp", "scp -f", "scp -t /files/uploaded", "scp -ft /files/nested/value.txt",
		"scp -f -- -f",
		"scp -f -Z /files/nested/value.txt", "scp -f /etc/passwd",
		"scp -f /files/nested/../nested/value.txt", "scp -f /files/%252e%252e/secret.txt",
		"scp -f /files/nested/value.txt /site/index.html", "scp -f /files/nested/value.txt; uname",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) { assertSCPRefused(t, connection, command, f.root) })
	}
	assertLocalContents(t, filepath.Join(f.root, "nested", "value.txt"), "download bytes\x00\xff\n")
	if _, err := os.Stat(filepath.Join(f.root, "uploaded")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SCP upload created an entry: %v", err)
	}
}

func assertSCPRefused(t *testing.T, connection *gossh.Client, command, root string) {
	t.Helper()
	session, err := connection.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close() //nolint:errcheck // test cleanup
	session.Stdin = strings.NewReader("\x00")
	output, err := session.CombinedOutput(command)
	if err == nil {
		t.Fatalf("command %q succeeded with %q", command, output)
	}
	if strings.Contains(string(output), root) || strings.Contains(string(output), "outside secret") {
		t.Fatalf("SCP refusal disclosed host data: %q", output)
	}
}

func assertSCPAcknowledge(t *testing.T, input io.Writer) {
	t.Helper()
	if _, err := input.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
}

func readSCPHeader(t *testing.T, reader *bufio.Reader, input io.Writer) string {
	t.Helper()
	header, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "T") {
		return header
	}
	assertSCPAcknowledge(t, input)
	header, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func assertPermission(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("error = %v; want SFTP permission denied", err)
	}
}

func assertNames(t *testing.T, client *sftp.Client, path string, want ...string) {
	t.Helper()
	entries, err := client.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ReadDir(%q) = %q; want %q", path, names, want)
	}
}

func assertRead(t *testing.T, client *sftp.Client, path, want string) {
	t.Helper()
	file, err := client.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	contents, err := io.ReadAll(file)
	if err != nil || string(contents) != want {
		t.Fatalf("Read(%q) = %q, %v; want %q", path, contents, err, want)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertLocalContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // only reads files in this test's temporary fixture
	if err != nil || string(contents) != want {
		t.Fatalf("local %q = %q, %v; want %q", path, contents, err, want)
	}
}
