package sshd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/wish/v2/testsession"
)

func TestAppCommandGrammar(t *testing.T) {
	command, err := parseCommand("app", false, true)
	if err != nil || command.Kind != CommandApp {
		t.Fatalf("app command: %#v %v", command, err)
	}
	for _, raw := range []string{"app extra", "app; id", "app --public"} {
		if _, err = parseCommand(raw, false, true); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err = parseCommand("app", true, true); err == nil {
		t.Fatal("app RPC accepted a terminal")
	}
}
func TestAppSSHReadsJSONAndKeepsContextAfterInputEOF(t *testing.T) {
	server, config := sessionTestServer(t, func(ctx context.Context, s Session) (int, error) {
		if s.Command.Kind != CommandApp || s.AppInput == nil || s.In != nil {
			return 1, errors.New("app RPC routed to terminal")
		}
		raw, err := io.ReadAll(s.AppInput)
		if err != nil {
			return 1, err
		}
		if string(raw) != "{\"action\":\"list\"}\n" {
			return 1, errors.New("app JSON input changed")
		}
		if err = ctx.Err(); err != nil {
			return 1, errors.New("input EOF canceled app operation")
		}
		_, err = io.WriteString(s.Out, "{\"result\":{\"apps\":[]}}\n")
		return 0, err
	})
	client := testsession.New(t, server, config)
	client.Stdin = strings.NewReader("{\"action\":\"list\"}\n")
	var stdout bytes.Buffer
	client.Stdout = &stdout
	if err := client.Run("app"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "{\"result\":{\"apps\":[]}}\n" {
		t.Fatalf("app reply changed: %q", stdout.String())
	}
}
