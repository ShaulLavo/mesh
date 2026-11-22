package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const (
	mcpHTTPTokenBytes      = 32
	mcpHTTPTokenPrefix     = "mht1_" //nolint:gosec // Non-secret token format version prefix, not a credential.
	mcpHTTPOwnerNonceBytes = 16
	mcpHTTPTokenMaxBytes   = 8192
	mcpHTTPControlMaxBytes = 4096
	mcpHTTPControlTimeout  = 5 * time.Second
	mcpHTTPReadHeaderLimit = 10 * time.Second
	mcpHTTPIdleTimeout     = 60 * time.Second
	mcpHTTPMaxHeaderBytes  = 64 << 10
	mcpHTTPShutdownTimeout = 2 * time.Second
	mcpHTTPActionRotate    = "rotate"
	mcpHTTPModeOperation   = "mcp.serve"
	mcpHTTPRotateOp        = "mcp.token.rotate"
)

type mcpHTTPOptions struct {
	ListenAddr     string
	IdentityName   string
	AllowedOrigins []string
	TokenFile      string
}

type mcpHTTPTokenFile struct {
	SchemaVersion       int       `json:"schemaVersion"`
	Listener            string    `json:"listener"`
	Endpoint            string    `json:"endpoint"`
	Identity            string    `json:"identity"`
	IdentityFingerprint string    `json:"identityFingerprint"`
	OwnerNonce          string    `json:"ownerNonce"`
	PID                 int       `json:"pid"`
	Generation          uint64    `json:"generation"`
	Token               string    `json:"token"`
	RotatedAt           time.Time `json:"rotatedAt"`
}

type mcpHTTPTokenHandle struct {
	dir        *os.File
	baseName   string
	path       string
	authority  string
	identity   string
	ownerNonce string
	dev        uint64
	ino        uint64
}

type mcpHTTPControlRequest struct {
	Action   string `json:"action"`
	Listener string `json:"listener"`
	Identity string `json:"identity"`
}

type mcpHTTPControlResponse struct {
	OK    bool                         `json:"ok"`
	Error string                       `json:"error,omitempty"`
	Data  *agentapi.MCPTokenRotateData `json:"data,omitempty"`
}

type mcpHTTPLiveState struct {
	authority   string
	endpoint    string
	identity    appcaller.Identity
	tokenHandle *mcpHTTPTokenHandle
	now         func() time.Time
	mcpSrv      *mcpnative.HTTPServer
	rotateMu    sync.Mutex
	generation  uint64
}

func (a *application) mcpTokenCommand() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage bearer tokens for the native loopback MCP HTTP server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	tokenCmd.SetOut(a.dependencies.Stdout)
	tokenCmd.SetErr(a.dependencies.Stderr)
	tokenCmd.AddCommand(a.mcpTokenRotateCommand())
	return tokenCmd
}

func (a *application) mcpTokenRotateCommand() *cobra.Command {
	var listenAddr string
	var identityName string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the bearer token for a running loopback MCP HTTP listener",
		Long: "Replace the bearer token stored for one running mesh mcp --http listener and\n" +
			"retire all active streams authenticated with the previous token. The token\n" +
			"value is written only to the private token file and is never printed or logged.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMCPTokenRotate(cmd, listenAddr, identityName, asJSON)
		},
	}
	cmd.Flags().StringVar(&listenAddr, "http", "", "exact loopback host:port of the running listener")
	cmd.Flags().StringVar(&identityName, "identity", "", "local application identity bound to the listener")
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

func (a *application) runMCPTokenRotate(cmd *cobra.Command, listenAddr, identityName string, asJSON bool) error {
	authority, err := parseLoopbackListenAddr(listenAddr, false)
	if err != nil {
		return agentapi.Usage(mcpHTTPRotateOp, err.Error())
	}
	if err := appcaller.ValidName(strings.TrimSpace(identityName)); err != nil {
		return agentapi.Usage(mcpHTTPRotateOp, err.Error())
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return executionBoundaryError("locate state directory", err)
	}
	sockPath, err := mcpHTTPControlSocketPath(stateDir, authority, identityName)
	if err != nil {
		return executionBoundaryError("resolve HTTP control socket", err)
	}
	result, err := sendMCPHTTPRotate(cmd.Context(), sockPath, authority, identityName)
	if err != nil {
		return executionBoundaryError("rotate MCP HTTP token", err)
	}
	if asJSON {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: result})
	}
	_, err = fmt.Fprintf(
		cmd.OutOrStdout(),
		"Rotated token for %s (identity: %s, generation: %d, retired streams: %d, token file: %s)\n",
		result.Endpoint,
		result.Identity,
		result.Generation,
		result.RetiredStreams,
		result.TokenFile,
	)
	return executionBoundaryError("print token rotation result", err)
}

func parseLoopbackListenAddr(raw string, allowZeroPort bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("--http requires a loopback host:port such as 127.0.0.1:8765")
	}
	host, portText, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid --http address %q: expected loopback host:port such as 127.0.0.1:8765", raw)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.IsLoopback() {
		return "", fmt.Errorf("--http host %q must be a literal loopback IP such as 127.0.0.1 or ::1", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 || (!allowZeroPort && port == 0) {
		return "", fmt.Errorf("invalid --http port %q", portText)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func generateMCPHTTPToken() (string, error) {
	var raw [mcpHTTPTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate MCP HTTP bearer token: %w", err)
	}
	return mcpHTTPTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func generateOwnerNonce() (string, error) {
	var raw [mcpHTTPOwnerNonceBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate MCP HTTP listener nonce: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func mcpHTTPControlSocketPath(stateDir, authority, identityName string) (string, error) {
	sum := sha256.Sum256([]byte("mesh-mcp-http-v1\x00" + authority + "\x00" + identityName))
	path := filepath.Join(stateDir, "h-"+hex.EncodeToString(sum[:6])+".s")
	if err := paths.ValidateSocketPath(path); err != nil {
		return "", fmt.Errorf("MCP HTTP control socket path: %w", err)
	}
	return path, nil
}

func mcpHTTPDefaultTokenPath(stateDir, authority, identityName string) string {
	sum := sha256.Sum256([]byte("mesh-mcp-http-token-v1\x00" + authority + "\x00" + identityName))
	return filepath.Join(stateDir, "mcp", "http", identityName+"-"+hex.EncodeToString(sum[:6])+".json")
}

func resolveTokenFilePath(stateDir, authority, identityName, override string) (string, error) {
	if strings.TrimSpace(override) == "" {
		return mcpHTTPDefaultTokenPath(stateDir, authority, identityName), nil
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		return "", fmt.Errorf("resolve --token-file %q: %w", override, err)
	}
	base := filepath.Base(abs)
	if base == "" || base == "." || base == ".." || base == string(os.PathSeparator) {
		return "", fmt.Errorf("invalid --token-file %q: must name a file", override)
	}
	return abs, nil
}

// canonicalizeTokenParentDir resolves symlinks in the existing parent prefix of
// dir once, so ordinary paths such as /tmp on macOS or a linked workspace root
// like /work/repos work. Not-yet-created suffix directories stay literal, and
// the token filename itself is never passed here. Everything after this walks
// the canonical path with O_NOFOLLOW descriptors so a mid-operation symlink
// swap is refused.
func canonicalizeTokenParentDir(dir string) (string, error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("token directory %q must be absolute", dir)
	}
	var tail []string
	cur := clean
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve token directory %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("resolve token directory %s: %w", cur, err)
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// openTokenDirNoFollow walks every directory component of a canonical absolute
// path from "/" without following symlinks and returns a held directory
// descriptor verified to be a directory.
func openTokenDirNoFollow(canonicalDir string) (*os.File, error) {
	clean := filepath.Clean(canonicalDir)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("token directory %q must be absolute", canonicalDir)
	}
	sep := string(os.PathSeparator)
	rest := strings.TrimPrefix(clean, sep)
	var parts []string
	if rest != "" {
		parts = strings.Split(rest, sep)
	}
	fd, err := unix.Open(sep, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open token root %s: %w", sep, err)
	}
	for _, part := range parts {
		next, stepErr := openOrCreateDirComponent(fd, part)
		_ = unix.Close(fd)
		if stepErr != nil {
			return nil, fmt.Errorf("open token directory %s: %w", clean, stepErr)
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), clean)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect token directory %s: %w", clean, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = file.Close()
		return nil, fmt.Errorf("token parent %s is not a directory", clean)
	}
	return file, nil
}

func openOrCreateDirComponent(parentFD int, name string) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, fmt.Errorf("open directory component %q without following symlinks: %w", name, err)
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("create directory component %q: %w", name, err)
	}
	fd, err = unix.Openat(parentFD, name, flags, 0)
	if err != nil {
		return -1, fmt.Errorf("open created directory component %q: %w", name, err)
	}
	return fd, nil
}

func claimPrivateTokenFile(path string, record mcpHTTPTokenFile) (*mcpHTTPTokenHandle, error) {
	canonicalDir, err := canonicalizeTokenParentDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	dirFile, err := openTokenDirNoFollow(canonicalDir)
	if err != nil {
		return nil, err
	}
	baseName := filepath.Base(path)
	canonicalPath := filepath.Join(canonicalDir, baseName)
	dirFD := int(dirFile.Fd())

	tempName, dev, ino, err := writeTempTokenInDir(dirFD, record)
	if err != nil {
		_ = dirFile.Close()
		return nil, err
	}
	if err := unix.Linkat(dirFD, tempName, dirFD, baseName, 0); err != nil {
		_ = unix.Unlinkat(dirFD, tempName, 0)
		_ = dirFile.Close()
		if errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("token file %s already exists; choose a fresh path or stop the existing listener", canonicalPath)
		}
		return nil, fmt.Errorf("claim token file %s: %w", canonicalPath, err)
	}
	_ = unix.Unlinkat(dirFD, tempName, 0)
	_ = unix.Fsync(dirFD)

	return &mcpHTTPTokenHandle{
		dir:        dirFile,
		baseName:   baseName,
		path:       canonicalPath,
		authority:  record.Listener,
		identity:   record.Identity,
		ownerNonce: record.OwnerNonce,
		dev:        dev,
		ino:        ino,
	}, nil
}

func writeTempTokenInDir(dirFD int, record mcpHTTPTokenFile) (tempName string, dev, ino uint64, err error) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", 0, 0, fmt.Errorf("encode MCP HTTP token file: %w", err)
	}
	data = append(data, '\n')
	suffix, err := generateOwnerNonce()
	if err != nil {
		return "", 0, 0, err
	}
	tempName = ".mcp-token-" + suffix
	fd, err := unix.Openat(dirFD, tempName, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", 0, 0, fmt.Errorf("create private token file: %w", err)
	}
	file := os.NewFile(uintptr(fd), tempName)
	var st unix.Stat_t
	_, writeErr := file.Write(data)
	statErr := unix.Fstat(fd, &st)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, statErr, syncErr, closeErr); err != nil {
		_ = unix.Unlinkat(dirFD, tempName, 0)
		return "", 0, 0, fmt.Errorf("write private token file: %w", err)
	}
	return tempName, uint64(st.Dev), st.Ino, nil //nolint:unconvert // Dev width differs across Linux and Darwin.
}

func (h *mcpHTTPTokenHandle) verifyOwned() error {
	dirFD := int(h.dir.Fd())
	fd, err := unix.Openat(dirFD, h.baseName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open owned token file %s: %w", h.path, err)
	}
	file := os.NewFile(uintptr(fd), h.baseName)
	defer func() { _ = file.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat owned token file %s: %w", h.path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 || uint64(st.Dev) != h.dev || st.Ino != h.ino { //nolint:unconvert // Dev width differs across Linux and Darwin.
		return fmt.Errorf("token file %s is no longer owned by this listener", h.path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, mcpHTTPTokenMaxBytes+1))
	if err != nil || len(raw) > mcpHTTPTokenMaxBytes {
		return fmt.Errorf("read owned token file %s: %w", h.path, err)
	}
	var rec mcpHTTPTokenFile
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("decode owned token file %s: %w", h.path, err)
	}
	if rec.PID != os.Getpid() || rec.Listener != h.authority || rec.Identity != h.identity || rec.OwnerNonce != h.ownerNonce {
		return fmt.Errorf("token file %s does not match this listener's ownership record", h.path)
	}
	return nil
}

func (h *mcpHTTPTokenHandle) rotate(record mcpHTTPTokenFile) error {
	if err := h.verifyOwned(); err != nil {
		return err
	}
	dirFD := int(h.dir.Fd())
	record.OwnerNonce = h.ownerNonce
	tempName, dev, ino, err := writeTempTokenInDir(dirFD, record)
	if err != nil {
		return err
	}
	if err := unix.Renameat(dirFD, tempName, dirFD, h.baseName); err != nil {
		_ = unix.Unlinkat(dirFD, tempName, 0)
		return fmt.Errorf("replace owned token file %s: %w", h.path, err)
	}
	_ = unix.Fsync(dirFD)
	h.dev = dev
	h.ino = ino
	return nil
}

func (h *mcpHTTPTokenHandle) closeAndRemove() {
	if h == nil || h.dir == nil {
		return
	}
	if h.verifyOwned() == nil {
		_ = unix.Unlinkat(int(h.dir.Fd()), h.baseName, 0)
		_ = unix.Fsync(int(h.dir.Fd()))
	}
	_ = h.dir.Close()
	h.dir = nil
}

func (a *application) runMCPHTTP(cmd *cobra.Command, opts mcpHTTPOptions) error {
	listenAddr, err := parseLoopbackListenAddr(opts.ListenAddr, true)
	if err != nil {
		return agentapi.Usage(mcpHTTPModeOperation, err.Error())
	}
	identityName := strings.TrimSpace(opts.IdentityName)
	if identityName == "" {
		return agentapi.Usage(mcpHTTPModeOperation, "--http requires --identity NAME; create one with mesh mcp identity create NAME.")
	}
	identity, err := loadApplicationIdentity(mcpHTTPModeOperation, identityName)
	if err != nil {
		return err
	}
	for _, origin := range opts.AllowedOrigins {
		if err := mcpnative.ValidateLoopbackOrigin(origin); err != nil {
			return agentapi.Usage(mcpHTTPModeOperation, err.Error())
		}
	}
	return a.serveMCPHTTP(cmd, listenAddr, identity, opts)
}

func (a *application) serveMCPHTTP(cmd *cobra.Command, listenAddr string, identity appcaller.Identity, opts mcpHTTPOptions) error {
	// Keep standard library logs off stdout; never log the bearer token.
	log.SetOutput(cmd.ErrOrStderr())
	stateDir, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("locate state directory: %w", err)
	}
	tcpLn, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
	}
	defer func() { _ = tcpLn.Close() }()

	authority := tcpLn.Addr().String()
	endpoint := "http://" + authority + mcpnative.DefaultHTTPPath
	tokenPath, err := resolveTokenFilePath(stateDir, authority, identity.Name, opts.TokenFile)
	if err != nil {
		return agentapi.Usage(mcpHTTPModeOperation, err.Error())
	}
	sockPath, err := mcpHTTPControlSocketPath(stateDir, authority, identity.Name)
	if err != nil {
		return err
	}
	controlLn, err := listenMCPHTTPControl(sockPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = controlLn.Close()
		_ = os.Remove(sockPath)
	}()

	return a.runBoundMCPHTTP(cmd, tcpLn, controlLn, authority, endpoint, tokenPath, identity, opts.AllowedOrigins)
}

func (a *application) runBoundMCPHTTP(
	cmd *cobra.Command,
	tcpLn, controlLn net.Listener,
	authority, endpoint, tokenPath string,
	identity appcaller.Identity,
	allowedOrigins []string,
) error {
	nowFn := a.dependencies.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	initialToken, err := generateMCPHTTPToken()
	if err != nil {
		return err
	}
	ownerNonce, err := generateOwnerNonce()
	if err != nil {
		return err
	}
	initialRecord := mcpHTTPTokenFile{
		SchemaVersion:       1,
		Listener:            authority,
		Endpoint:            endpoint,
		Identity:            identity.Name,
		IdentityFingerprint: identity.Credential.Fingerprint(),
		OwnerNonce:          ownerNonce,
		PID:                 os.Getpid(),
		Generation:          1,
		Token:               initialToken,
		RotatedAt:           nowFn().UTC().Truncate(time.Second),
	}
	tokenHandle, err := claimPrivateTokenFile(tokenPath, initialRecord)
	if err != nil {
		return agentapi.Usage(mcpHTTPModeOperation, err.Error())
	}
	defer tokenHandle.closeAndRemove()

	pool := newMCPHostPool(a.dependencies.DialPassive, nowFn)
	defer pool.Close()
	caller := protocol.CallerCredential(identity.Credential.Encode())
	controls := newMCPControls(a, pool, caller)
	mcpSrv, err := mcpnative.NewHTTPServer(mcpnative.HTTPServerOptions{
		ServerOptions: mcpnative.ServerOptions{
			Version:    release.Metadata().Version,
			Controls:   controls,
			Stderr:     cmd.ErrOrStderr(),
			OnShutdown: pool.Close,
		},
		Authority:      authority,
		Path:           mcpnative.DefaultHTTPPath,
		AllowedOrigins: allowedOrigins,
		Token:          initialToken,
	})
	if err != nil {
		return fmt.Errorf("initialize MCP HTTP server: %w", err)
	}

	live := &mcpHTTPLiveState{
		authority:   authority,
		endpoint:    endpoint,
		identity:    identity,
		tokenHandle: tokenHandle,
		now:         nowFn,
		mcpSrv:      mcpSrv,
		generation:  1,
	}
	return serveHTTPUntilSignal(cmd.Context(), cmd.ErrOrStderr(), tcpLn, controlLn, live)
}

func serveHTTPUntilSignal(ctx context.Context, stderr io.Writer, tcpLn, controlLn net.Listener, live *mcpHTTPLiveState) error {
	runCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	httpSrv := &http.Server{
		Handler:           live.mcpSrv,
		ReadHeaderTimeout: mcpHTTPReadHeaderLimit,
		IdleTimeout:       mcpHTTPIdleTimeout,
		MaxHeaderBytes:    mcpHTTPMaxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	live.mcpSrv.Attach(httpSrv)

	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		live.serveControlLoop(controlLn)
	}()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- httpSrv.Serve(tcpLn)
	}()

	_, _ = fmt.Fprintf(stderr, "mesh mcp: listening on %s (identity: %s, token file: %s)\n", live.endpoint, live.identity.Name, live.tokenHandle.path)

	var serveErr error
	select {
	case <-runCtx.Done():
	case serveErr = <-serveErrCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpHTTPShutdownTimeout)
	defer cancel()
	_ = controlLn.Close()
	_ = live.mcpSrv.Shutdown(shutdownCtx)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		_ = httpSrv.Close()
	}
	<-controlDone
	if serveErr == nil {
		serveErr = <-serveErrCh
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve MCP HTTP: %w", serveErr)
	}
	return nil
}

func listenMCPHTTPControl(sockPath string) (net.Listener, error) {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create MCP HTTP control socket directory %s: %w", dir, err)
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on MCP HTTP control socket %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod MCP HTTP control socket %s: %w", sockPath, err)
	}
	return ln, nil
}

func (l *mcpHTTPLiveState) serveControlLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		l.handleControlConn(conn)
	}
}

func (l *mcpHTTPLiveState) handleControlConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(mcpHTTPControlTimeout))
	raw, err := readControlLine(conn)
	if err != nil {
		writeControlResponse(conn, mcpHTTPControlResponse{Error: "malformed control request"})
		return
	}
	var req mcpHTTPControlRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Action != mcpHTTPActionRotate {
		writeControlResponse(conn, mcpHTTPControlResponse{Error: "unsupported control action"})
		return
	}
	if req.Listener != l.authority || req.Identity != l.identity.Name {
		writeControlResponse(conn, mcpHTTPControlResponse{Error: "listener or identity does not match running MCP HTTP server"})
		return
	}
	data, err := l.rotate()
	if err != nil {
		writeControlResponse(conn, mcpHTTPControlResponse{Error: err.Error()})
		return
	}
	writeControlResponse(conn, mcpHTTPControlResponse{OK: true, Data: &data})
}

func (l *mcpHTTPLiveState) rotate() (agentapi.MCPTokenRotateData, error) {
	l.rotateMu.Lock()
	defer l.rotateMu.Unlock()

	newToken, err := generateMCPHTTPToken()
	if err != nil {
		return agentapi.MCPTokenRotateData{}, err
	}
	nextGen := l.generation + 1
	rotatedAt := l.now().UTC().Truncate(time.Second)
	record := mcpHTTPTokenFile{
		SchemaVersion:       1,
		Listener:            l.authority,
		Endpoint:            l.endpoint,
		Identity:            l.identity.Name,
		IdentityFingerprint: l.identity.Credential.Fingerprint(),
		PID:                 os.Getpid(),
		Generation:          nextGen,
		Token:               newToken,
		RotatedAt:           rotatedAt,
	}
	if err := l.tokenHandle.rotate(record); err != nil {
		return agentapi.MCPTokenRotateData{}, err
	}
	gen, retired, err := l.mcpSrv.RotateToken(newToken)
	if err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("apply rotated token: %w", err)
	}
	l.generation = gen
	return agentapi.MCPTokenRotateData{
		Listener:       l.authority,
		Endpoint:       l.endpoint,
		Identity:       l.identity.Name,
		Generation:     gen,
		RetiredStreams: retired,
		TokenFile:      l.tokenHandle.path,
		RotatedAt:      rotatedAt,
	}, nil
}

func sendMCPHTTPRotate(ctx context.Context, sockPath, authority, identityName string) (agentapi.MCPTokenRotateData, error) {
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, mcpHTTPControlTimeout)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "unix", sockPath)
	if err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("connect to MCP HTTP listener %s for identity %s: %w", authority, identityName, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(mcpHTTPControlTimeout))
	payload, err := json.Marshal(mcpHTTPControlRequest{
		Action:   mcpHTTPActionRotate,
		Listener: authority,
		Identity: identityName,
	})
	if err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("encode rotate request: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("send rotate request: %w", err)
	}
	raw, err := readControlLine(conn)
	if err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("read rotate response: %w", err)
	}
	var resp mcpHTTPControlResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return agentapi.MCPTokenRotateData{}, fmt.Errorf("decode rotate response: %w", err)
	}
	if !resp.OK || resp.Data == nil {
		if resp.Error == "" {
			resp.Error = "rotation rejected"
		}
		return agentapi.MCPTokenRotateData{}, errors.New(resp.Error)
	}
	return *resp.Data, nil
}

func readControlLine(r io.Reader) ([]byte, error) {
	reader := bufio.NewReader(io.LimitReader(r, mcpHTTPControlMaxBytes))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read control line: %w", err)
	}
	return line, nil
}

func writeControlResponse(w io.Writer, resp mcpHTTPControlResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}
