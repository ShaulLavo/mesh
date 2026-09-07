package updatebootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
)

func testRequest() Request {
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	manifest := release.Manifest{Schema: 1, Version: "v0.2.0", Commit: strings.Repeat("a", 40), Compatibility: release.Compatibility{StateReadMin: 7, StateReadMax: 7, StateWrite: 7, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64)})
	}
	return Request{ID: "operation-1", TargetID: key, CoordinatorID: key, Generation: 1, Manifest: manifest}
}

func TestBootstrapCommandUsesFixedOriginAndStructuredArguments(t *testing.T) {
	request := testRequest()
	args, err := Command(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(args[6])
	if err != nil || decoded.Manifest.Digest() != request.Manifest.Digest() {
		t.Fatalf("request roundtrip: %+v %v", decoded, err)
	}
	request.ID = "bad;touch /tmp/injected"
	if _, err = Command(request); err == nil {
		t.Fatal("unsafe operation ID accepted")
	}
	request = testRequest()
	request.Manifest.Version = "v0.2.0/../../other"
	if _, err = Command(request); err == nil {
		t.Fatal("nonrelease selector accepted")
	}
}

func TestFixedDownloadScriptVerifiesOfficialArchiveAndApprovedBinary(t *testing.T) {
	root := t.TempDir()
	binary := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$MESH_BOOTSTRAP_CAPTURE\"\n")
	archive := testArchive(t, binary)
	request := testRequest()
	for index := range request.Manifest.Artifacts {
		request.Manifest.Artifacts[index].SHA256 = hashBytes(archive)
		request.Manifest.Artifacts[index].BinarySHA256 = hashBytes(binary)
	}
	if err := os.WriteFile(filepath.Join(root, "archive"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	var checksums strings.Builder
	for _, artifact := range request.Manifest.Artifacts {
		fmt.Fprintf(&checksums, "%s  %s\n", artifact.SHA256, artifact.Archive)
	}
	if err := os.WriteFile(filepath.Join(root, "checksums.txt"), []byte(checksums.String()), 0600); err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	curl := `#!/bin/sh
set -eu
previous=
address=
for argument do
  case "$argument" in https://*) address=$argument ;; esac
  if [ "$previous" = -o ]; then destination=$argument; fi
  previous=$argument
done
printf '%s\n' "$address" >> "$MESH_BOOTSTRAP_AUDIT"
case "$address" in
  https://github.com/ShaulLavo/mesh/releases/download/v0.2.0/checksums.txt) cp "$MESH_BOOTSTRAP_FIXTURE/checksums.txt" "$destination" ;;
  https://github.com/ShaulLavo/mesh/releases/download/v0.2.0/mesh_*.tar.gz) cp "$MESH_BOOTSTRAP_FIXTURE/archive" "$destination" ;;
  *) exit 88 ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "curl"), []byte(curl), 0700); err != nil { //nolint:gosec // isolated executable download fixture
		t.Fatal(err)
	}
	run := func(request Request) error {
		args, err := Command(request)
		if err != nil {
			return err
		}
		command := exec.Command(args[0], args[1:]...) //nolint:gosec // validated fixed bootstrap script executed against isolated download fixtures
		command.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "MESH_UPDATE_CACHE_DIR="+filepath.Join(root, "cache with spaces"), "MESH_UPDATE_REQUIRED_MOUNT=", "MESH_BOOTSTRAP_FIXTURE="+root, "MESH_BOOTSTRAP_CAPTURE="+filepath.Join(root, "capture"), "MESH_BOOTSTRAP_AUDIT="+filepath.Join(root, "audit"))
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, output)
		}
		return nil
	}
	if err := run(request); err != nil {
		t.Fatal(err)
	}
	capture, err := os.ReadFile(filepath.Join(root, "capture")) //nolint:gosec // fixed output path beneath the test directory
	if err != nil || !strings.HasPrefix(string(capture), "update-bootstrap\n--request-base64\n") {
		t.Fatalf("verified command = %s %v", capture, err)
	}
	if err = os.WriteFile(filepath.Join(root, "archive"), []byte("corrupt archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run(request); err == nil {
		t.Fatal("corrupt official archive executed")
	}
}

func TestLegacyInspectorReadsExistingSchemaWithoutMigrating(t *testing.T) {
	state := t.TempDir()
	createLegacyDatabase(t, state)
	build := release.Current()
	listener, err := net.Listen("unix", filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = protocol.NewReader(conn).ReadFrame()
		_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: "test-host", Build: &build}})
	}()
	observed, err := Inspect(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Health.Build.Digest != build.Digest || observed.Health.Build.StateVersion != 7 {
		t.Fatalf("observed = %+v", observed)
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("inspection ran migrations: %d tables", count)
	}
}

func TestReadOnlyInspectionNeverCreatesMissingDatabase(t *testing.T) {
	state := t.TempDir()
	if _, err := readStateVersion(context.Background(), state); err == nil {
		t.Fatal("missing state accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "mesh.db")); !os.IsNotExist(err) {
		t.Fatalf("inspection created a database: %v", err)
	}
}

func TestLegacyLinkerVersionAndRequestBounds(t *testing.T) {
	var build release.Build
	applyLegacySetting(&build, "-ldflags", "-s -w -buildid= -X github.com/shaul/mesh/internal/bootstrap.releaseVersion=v0.1.38")
	if build.Version != "v0.1.38" {
		t.Fatalf("legacy version = %q", build.Version)
	}
	if _, err := Decode(strings.Repeat("a", (256<<10)+1)); err == nil {
		t.Fatal("unbounded request accepted")
	}
	data, _ := json.Marshal(testRequest())
	data = append(data, []byte(" {}")...)
	if _, err := Decode(base64.RawURLEncoding.EncodeToString(data)); err == nil {
		t.Fatal("trailing request data accepted")
	}
}

func createLegacyDatabase(t *testing.T, state string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(state, "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec("CREATE TABLE goose_db_version(version_id INTEGER,is_applied BOOLEAN); INSERT INTO goose_db_version VALUES(7,1)"); err != nil {
		t.Fatal(err)
	}
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func testArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "mesh", Mode: 0700, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
