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
	"time"

	"github.com/shaul/mesh/internal/protocol"
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

func Trust(stateDir, id string, ifEmpty bool) error {
	if _, err := PublicKey(id); err != nil {
		return err
	}
	store, err := OpenStore(stateDir)
	if err != nil {
		return err
	}
	unlock, err := store.lock()
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(stateDir, "updates", "administrators.json")
	policy := Policy{}
	err = readJSON(path, &policy)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists := err == nil
	if exists && policy.Version != 1 {
		return errors.New("unsupported update administrator policy version")
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

func (a *Authority) allowed(id string) bool {
	if id == a.ID {
		return true
	}
	var policy Policy
	if readJSON(filepath.Join(a.StateDir, "updates", "administrators.json"), &policy) != nil || policy.Version != 1 || len(policy.Keys) > MaximumHosts {
		return false
	}
	for _, key := range policy.Keys {
		if key == id {
			return true
		}
	}
	return false
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
	if message.Target != a.ID || !a.allowed(message.Actor) {
		return protocol.Control{}, true, errors.New("update administrator is not enrolled on this host")
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
