package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
)

type configurationProcessConfig struct {
	State string
	Host  Host
	Key   ed25519.PrivateKey
	Fleet Fleet
	Fault string
}
type configurationProcess struct {
	config  configurationProcessConfig
	command *exec.Cmd
	client  Client
}

func (p *configurationProcess) start(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(p.config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/proc/self/exe", "-test.run=^TestConfigurationProcessHelper$")
	command.Env = append(testenv.ForProcess(p.config.State), "MESH_CONFIGURATION_PROCESS=1")
	command.Stdin = bytes.NewReader(data)
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	p.command = command
	t.Cleanup(func() { p.stop(t) })
	ready := make(chan error, 1)
	go func() { var marker string; ready <- json.NewDecoder(stdout).Decode(&marker) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configuration fixture did not listen")
	}
}
func (p *configurationProcess) stop(t *testing.T) {
	t.Helper()
	if p.command == nil {
		return
	}
	command := p.command
	p.command = nil
	_ = command.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("configuration fixture did not stop")
	}
}
func (p *configurationProcess) call(t *testing.T, action string, input, output any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return p.client.Call(ctx, p.config.Host, action, input, output)
}
func TestConfigurationProcessesRecoverIssuedAndRecipientIntent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated process fault fixture uses the Linux test executable")
	}
	for _, fault := range []string{"before-write", "after-write"} {
		t.Run(fault, func(t *testing.T) { testConfigurationProcessRecovery(t, fault) })
	}
}
func testConfigurationProcessRecovery(t *testing.T, fault string) {
	processes := newConfigurationProcesses(t, fault)
	coordinator, recipient := processes[1], processes[0]
	var proposal ConfigurationProposal
	selection := ConfigurationSelection{Targets: []string{recipient.config.Host.ID}, UsageFeed: &FeedChange{}}
	if err := coordinator.call(t, "configuration.preview", selection, &proposal); err != nil {
		t.Fatal(err)
	}
	approval := ConfigurationApproval{CommandID: mustAutomaticID(t), Proposal: proposal}
	var status ConfigurationStatus
	if err := coordinator.call(t, "configuration.approve", approval, &status); err != nil {
		t.Fatal(err)
	}
	eventuallyConfiguration(t, func() bool {
		var receipt ConfigurationReceipt
		err := readJSON(filepath.Join(recipient.config.State, "updates", "config-receipt.json"), &receipt)
		return err == nil && receipt.State == configurationPrepared
	}, "recipient never durably prepared")
	recipient.stop(t)
	coordinator.stop(t)
	recipient.config.Fault = ""
	recipient.start(t)
	coordinator.start(t)
	if err := coordinator.call(t, "configuration.approve", approval, &status); err != nil || status.Revision != 1 {
		t.Fatalf("approval restart replay: %+v %v", status, err)
	}
	command := ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: proposal.ID, ExpectedRevision: status.Revision}
	if err := coordinator.call(t, "configuration.retry", command, &status); err != nil {
		t.Fatal(err)
	}
	eventuallyConfiguration(t, func() bool {
		if err := coordinator.call(t, "configuration.status", struct {
			ID string `json:"id"`
		}{proposal.ID}, &status); err != nil {
			return false
		}
		return status.Change.Targets[0].State == configurationSaved
	}, "recipient intent did not recover")
	if status.Change.Targets[0].Receipt.Revision != 1 || status.Change.Attempt != 2 {
		t.Fatal("restart duplicated recipient revision", status)
	}
	recipient.stop(t)
	recipient.start(t)
	if err := coordinator.call(t, "configuration.retry", command, &status); err != nil || status.Change.Attempt != 2 {
		t.Fatal("retry replay allocated another attempt", err)
	}
	for _, process := range processes {
		process.stop(t)
	}
}
func eventuallyConfiguration(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal(message)
		}
	}
}
func newConfigurationProcesses(t *testing.T, fault string) []*configurationProcess {
	t.Helper()
	root := testenv.SocketTempDir(t)
	fleet := testFleet(t, 2)
	keys := make([]ed25519.PrivateKey, 2)
	for index := range fleet.Members {
		fleet.Members[index].ID, keys[index] = testIdentity(t)
		fleet.Members[index].Endpoint = "unix://" + filepath.Join(root, fmt.Sprintf("c%d.sock", index))
	}
	owner, ownerKey := testIdentity(t)
	processes := make([]*configurationProcess, 0, 2)
	for index, host := range fleet.Members {
		state := filepath.Join(root, fmt.Sprintf("state%d", index))
		store, err := OpenStore(state)
		if err != nil {
			t.Fatal(err)
		}
		if err = Trust(t.Context(), state, owner, false); err != nil {
			t.Fatal(err)
		}
		if err = Trust(t.Context(), state, fleet.Members[1].ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err = store.SetConfigurationGrant(t.Context(), 0, host.ID, fleet.Members[1].ID, true, false); err != nil {
			t.Fatal(err)
		}
		cfg := configurationProcessConfig{State: state, Host: host, Key: keys[index], Fleet: fleet}
		if index == 0 {
			cfg.Fault = fault
		}
		process := &configurationProcess{config: cfg, client: Client{ID: owner, Key: ownerKey}}
		processes = append(processes, process)
		process.start(t)
	}
	return processes
}

type processConfigurationFile struct {
	root  *os.Root
	value FeedValue
	fault string
}

func (f *processConfigurationFile) Value() FeedValue { return f.value }
func (f *processConfigurationFile) Save(value FeedValue) error {
	if f.fault == "before-write" {
		os.Exit(41)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode fixture feed: %w", err)
	}
	if err = f.root.WriteFile("feed.json", data, 0600); err != nil {
		return fmt.Errorf("publish fixture feed: %w", err)
	}
	if f.fault == "after-write" {
		os.Exit(42)
	}
	f.value = value
	return nil
}
func (f *processConfigurationFile) Settle() error { return nil }
func TestConfigurationProcessHelper(t *testing.T) {
	if os.Getenv("MESH_CONFIGURATION_PROCESS") != "1" {
		return
	}
	var cfg configurationProcessConfig
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	store, err := OpenStore(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	var writer sync.Mutex
	apply := func(_ context.Context, operation func(FeedFile) error) error {
		writer.Lock()
		defer writer.Unlock()
		value, err := readProcessConfiguration(root)
		if err != nil {
			return err
		}
		return operation(&processConfigurationFile{root, value, cfg.Fault})
	}
	authority := &Authority{StateDir: cfg.State, ID: cfg.Host.ID, Key: cfg.Key}
	coordinator := &Coordinator{ID: cfg.Host.ID, Store: store, Trusted: authority.IsAdministrator, SavedFleet: func() (Fleet, error) { return cfg.Fleet, nil }, Remote: Client{ID: cfg.Host.ID, Key: cfg.Key}}
	authority.Handle = func(ctx context.Context, actor VerifiedActor, action string, data json.RawMessage) (any, error) {
		return configurationChildControl(ctx, coordinator, authority, actor, action, data, apply)
	}
	socket := strings.TrimPrefix(cfg.Host.Endpoint, "unix://")
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); coordinator.Run(ctx, nil) }()
	defer func() { cancel(); <-done }()
	if err = json.NewEncoder(os.Stdout).Encode("ready"); err != nil {
		t.Fatal(err)
	}
	for {
		stream, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		go serveConfigurationChild(stream, authority)
	}
}
func serveConfigurationChild(stream net.Conn, authority *Authority) {
	connection, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	defer func() { _ = connection.Close() }()
	for range 2 {
		if err := exchangeDownloadPlan(context.Background(), connection, authority); err != nil {
			return
		}
	}
}
func configurationChildControl(ctx context.Context, c *Coordinator, a *Authority, actor VerifiedActor, action string, data json.RawMessage, writer ConfigurationWriter) (any, error) {
	switch action {
	case "configuration.preview":
		var selection ConfigurationSelection
		if err := DecodeConfiguration(data, &selection); err != nil {
			return nil, err
		}
		return c.PreviewConfiguration(ctx, actor, selection)
	case "configuration.approve":
		var approval ConfigurationApproval
		if err := DecodeConfiguration(data, &approval); err != nil {
			return nil, err
		}
		return c.ApproveConfiguration(ctx, actor, approval)
	case "configuration.retry":
		var command ConfigurationCommand
		if err := DecodeConfiguration(data, &command); err != nil {
			return nil, err
		}
		return c.ChangeConfiguration(ctx, actor, "retry", command)
	case "configuration.status":
		var selector struct {
			ID string `json:"id"`
		}
		if err := DecodeConfiguration(data, &selector); err != nil {
			return nil, err
		}
		return c.Store.ConfigurationStatus(ctx, selector.ID)
	case "configuration.snapshot":
		return c.Store.ConfigurationSnapshot(ctx, c.ID, writer, nil)
	case "configuration.put":
		var request ConfigurationPut
		if err := DecodeConfiguration(data, &request); err != nil {
			return nil, err
		}
		return c.Store.PutConfiguration(ctx, actor, c.ID, a.IsAdministrator, writer, request)
	default:
		return nil, errors.New("unknown fixture action")
	}
}

func readProcessConfiguration(root *os.Root) (FeedValue, error) {
	var value FeedValue
	data, err := root.ReadFile("feed.json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return value, fmt.Errorf("read fixture feed: %w", err)
	}
	if len(data) == 0 {
		return value, nil
	}
	if err = json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("decode fixture feed: %w", err)
	}
	return value, nil
}
