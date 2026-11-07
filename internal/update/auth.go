// Package update coordinates durable, explicitly approved fleet releases.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
	"golang.org/x/sys/unix"
)

const ControlType = "update.control"
const maximumMessage = 2 << 20

type Message struct {
	Action    string          `json:"action"`
	Actor     string          `json:"actor"`
	Target    string          `json:"target"`
	Nonce     string          `json:"nonce"`
	Data      json.RawMessage `json:"data,omitempty"`
	Problem   string          `json:"problem,omitempty"`
	Signature string          `json:"signature"`
}

func (m Message) signedBytes() []byte {
	m.Signature = ""
	data, _ := json.Marshal(m)
	return append([]byte("mesh-update-v1\x00"), data...)
}

func (m *Message) Sign(key ed25519.PrivateKey) {
	m.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, m.signedBytes()))
}

func (m Message) Verify(id string) error {
	key, err := PublicKey(id)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(key, m.signedBytes(), signature) {
		return errors.New("invalid update signature")
	}
	return nil
}

type Policy struct {
	Version int      `json:"version"`
	Keys    []string `json:"keys"`
}

func Trust(ctx context.Context, stateDir, id string, ifEmpty bool) error {
	err := access.WithClassWriter(ctx, stateDir, 1500*time.Millisecond, func(ctx context.Context) error { return trustOwned(ctx, stateDir, id, ifEmpty) })
	if err != nil {
		return fmt.Errorf("update: trust administrator: %w", err)
	}
	return nil
}
func trustOwned(ctx context.Context, stateDir, id string, ifEmpty bool) error {
	if _, err := PublicKey(id); err != nil {
		return err
	}
	store, err := OpenStore(stateDir)
	if err != nil {
		return err
	}
	unlock, err := lockTrust(ctx, store.directory)
	if err != nil {
		return err
	}
	defer unlock()
	registered, err := access.Registered(stateDir, access.Subject(id))
	if err != nil {
		return fmt.Errorf("update: inspect scoped registration: %w", err)
	}
	if registered {
		return errors.New("update: registered scoped identity cannot become an administrator")
	}
	return publishAdministrator(stateDir, id, ifEmpty)
}
func publishAdministrator(stateDir, id string, ifEmpty bool) error {
	path := filepath.Join(stateDir, "updates", "administrators.json")
	policy, err := readAdministratorPolicy(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists := err == nil
	if exists {
		if err := validateAdministratorPolicy(policy); err != nil {
			return err
		}
	}
	if !exists {
		policy.Version = 1
	}
	for _, existing := range policy.Keys {
		if existing == id {
			return nil
		}
	}
	if ifEmpty && exists {
		return errors.New("update policy already exists; enroll the coordinator locally")
	}
	if len(policy.Keys) >= MaximumHosts {
		return errors.New("too many update administrators")
	}
	policy.Keys = append(policy.Keys, id)
	return writeJSON(path, policy)
}

func lockTrust(ctx context.Context, directory string) (func(), error) {
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open administrator lock directory: %w", err)
	}
	dir := os.NewFile(uintptr(directoryFD), directory)
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), ".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("update: open administrator lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), ".lock")
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("update: inspect administrator writer lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Nlink != 1 || int64(stat.Uid) != int64(os.Geteuid()) {
		_ = file.Close()
		return nil, errors.New("update: administrator lock must be private, owned and regular")
	}
	if err := access.WaitWriter(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("update: acquire administrator writer: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
func AdministratorMembership(stateDir, id string) (bool, error) {
	if _, err := PublicKey(id); err != nil {
		return false, err
	}
	policy, err := readAdministratorPolicy(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := validateAdministratorPolicy(policy); err != nil {
		return false, err
	}
	for _, key := range policy.Keys {
		if key == id {
			return true, nil
		}
	}
	return false, nil
}
func validateAdministratorPolicy(policy Policy) error {
	if policy.Version != 1 || len(policy.Keys) > MaximumHosts || policy.Keys == nil {
		return errors.New("update: invalid administrator policy")
	}
	seen := make(map[string]bool, len(policy.Keys))
	for _, key := range policy.Keys {
		if _, err := PublicKey(key); err != nil {
			return err
		}
		if seen[key] {
			return errors.New("update: duplicate administrator identity")
		}
		seen[key] = true
	}
	return nil
}

func administratorDirectory(stateDir string) (*os.File, error) {
	fd, err := unix.Open(filepath.Join(stateDir, "updates"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open administrator directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), "updates")
	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("update: inspect administrator directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		_ = dir.Close()
		return nil, errors.New("update: administrator directory must be safely owned")
	}
	return dir, nil
}
func readAdministratorPolicy(stateDir string) (Policy, error) {
	var policy Policy
	dir, err := administratorDirectory(stateDir)
	if err != nil {
		return policy, err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), "administrators.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return policy, fmt.Errorf("update: open administrator policy: %w", err)
	}
	file := os.NewFile(uintptr(fd), "administrators.json")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return policy, fmt.Errorf("update: inspect administrator policy: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || stat.Mode&0444 == 0 || stat.Nlink != 1 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Size > maximumMessage {
		return policy, errors.New("update: unsafe administrator policy")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumMessage+1))
	if err != nil {
		return policy, fmt.Errorf("update: read administrator policy: %w", err)
	}
	if len(data) > maximumMessage {
		return policy, errors.New("update: administrator policy exceeds size limit")
	}
	return decodeAdministratorPolicy(data)
}
func decodeAdministratorPolicy(data []byte) (Policy, error) {
	var policy Policy
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return policy, errors.New("update: administrator policy object required")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return policy, fmt.Errorf("update: read policy field: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] || key != "version" && key != "keys" {
			return policy, errors.New("update: unknown or duplicate policy field")
		}
		seen[key] = true
		if err := decodeAdministratorField(decoder, key, &policy); err != nil {
			return policy, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return policy, fmt.Errorf("update: close administrator policy: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return policy, errors.New("update: trailing administrator policy data")
	}
	if err := validateAdministratorPolicy(policy); err != nil {
		return policy, err
	}
	return policy, nil
}
func decodeAdministratorField(decoder *json.Decoder, key string, policy *Policy) error {
	var target any = &policy.Version
	if key == "keys" {
		target = &policy.Keys
	}
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("update: decode administrator field: %w", err)
	}
	return nil
}

type challenge struct {
	actor   string
	expires time.Time
}

type Authority struct {
	StateDir   string
	ID         string
	Key        ed25519.PrivateKey
	Handle     func(context.Context, string, json.RawMessage) (any, error)
	mu         sync.Mutex
	challenges map[string]challenge
}

func (a *Authority) IsAdministrator(id string) bool {
	if id == a.ID {
		return true
	}
	member, err := AdministratorMembership(a.StateDir, id)
	return err == nil && member
}

func (a *Authority) challenge(actor string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.challenges == nil {
		a.challenges = make(map[string]challenge)
	}
	for nonce, entry := range a.challenges {
		if time.Now().After(entry.expires) {
			delete(a.challenges, nonce)
		}
	}
	if len(a.challenges) >= MaximumHosts {
		return "", errors.New("too many pending update requests")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(random[:])
	a.challenges[nonce] = challenge{actor: actor, expires: time.Now().Add(30 * time.Second)}
	return nonce, nil
}

func (a *Authority) consume(message Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, exists := a.challenges[message.Nonce]
	if !exists || entry.actor != message.Actor || time.Now().After(entry.expires) {
		return errors.New("update challenge expired or already consumed")
	}
	delete(a.challenges, message.Nonce)
	return nil
}

func decodeMessage(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maximumMessage {
		return errors.New("invalid update message size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing update message data")
	}
	return nil
}

func (a *Authority) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != ControlType {
		return protocol.Control{}, false, nil
	}
	var message Message
	if err := decodeMessage(request.Update, &message); err != nil {
		return protocol.Control{}, true, err
	}
	if _, err := PublicKey(message.Actor); err != nil {
		return protocol.Control{}, true, err
	}
	if message.Target != a.ID || !a.IsAdministrator(message.Actor) {
		return protocol.Control{}, true, fmt.Errorf("update administrator is not enrolled on this host; run mesh update trust %s on this host", message.Actor)
	}
	if err := message.Verify(message.Actor); err != nil {
		return protocol.Control{}, true, err
	}
	response := Message{Action: message.Action, Actor: a.ID, Target: message.Actor, Nonce: message.Nonce}
	result, err := a.dispatch(ctx, message)
	if err != nil {
		response.Problem = err.Error()
	} else {
		response.Data, err = json.Marshal(result)
	}
	if err != nil && response.Problem == "" {
		return protocol.Control{}, true, err
	}
	response.Sign(a.Key)
	data, err := json.Marshal(response)
	return protocol.Control{Type: ControlType, RequestID: request.RequestID, Update: data}, true, err
}

func (a *Authority) dispatch(ctx context.Context, message Message) (any, error) {
	if message.Action == "challenge" {
		return a.challenge(message.Actor)
	}
	if err := a.consume(message); err != nil {
		return nil, err
	}
	if a.Handle == nil {
		return nil, fmt.Errorf("update handler unavailable")
	}
	return a.Handle(ctx, message.Action, message.Data)
}
