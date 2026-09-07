package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type Client struct {
	ID  string
	Key ed25519.PrivateKey
}

type RemoteError struct{ Problem string }

func (e *RemoteError) Error() string { return e.Problem }

func (c Client) Call(ctx context.Context, host Host, action string, input, output any) error {
	if err := host.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := dial(ctx, host)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	request := Message{Action: "challenge", Actor: c.ID, Target: host.ID, Nonce: hex.EncodeToString(random[:])}
	challenge, err := c.exchange(conn, host, request)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(challenge.Data, &request.Nonce); err != nil {
		return err
	}
	request.Action = action
	request.Data, err = json.Marshal(input)
	if err != nil {
		return err
	}
	response, err := c.exchange(conn, host, request)
	if err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(response.Data, output)
}

func (c Client) exchange(conn transport.Conn, host Host, message Message) (Message, error) {
	message.Sign(c.Key)
	raw, err := json.Marshal(message)
	if err != nil {
		return Message{}, err
	}
	payload, err := (protocol.Control{Type: ControlType, RequestID: message.Nonce, Update: raw}).Encode()
	if err != nil {
		return Message{}, err
	}
	if len(raw) > maximumMessage {
		return Message{}, errors.New("update message exceeds limit")
	}
	if err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return Message{}, err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return Message{}, err
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return Message{}, err
	}
	if control.Type == protocol.TypeError {
		return Message{}, &RemoteError{Problem: control.Message}
	}
	if frame.Kind != protocol.KindControl || control.Type != ControlType || control.RequestID != message.Nonce {
		return Message{}, errors.New("unexpected update response")
	}
	var response Message
	if err = decodeMessage(control.Update, &response); err != nil {
		return Message{}, err
	}
	if response.Actor != host.ID || response.Target != c.ID || response.Action != message.Action || response.Nonce != message.Nonce {
		return Message{}, errors.New("update response identity mismatch")
	}
	if err = response.Verify(host.ID); err != nil {
		return Message{}, err
	}
	if response.Problem != "" {
		return Message{}, &RemoteError{Problem: response.Problem}
	}
	return response, nil
}

func dial(ctx context.Context, host Host) (transport.Conn, error) {
	address, err := url.Parse(host.Endpoint)
	if err != nil {
		return nil, err
	}
	if address.Scheme != "unix" {
		return transport.DialOnce(ctx, host.Endpoint, transport.DialOptions{})
	}
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", address.Path)
	if err != nil {
		return nil, fmt.Errorf("dial update daemon: %w", err)
	}
	return transport.NewStreamConn(stream)
}
