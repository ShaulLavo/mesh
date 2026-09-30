package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appspkg "github.com/shaul/mesh/internal/apps"
)

func TestBrowserApprovalRefusesNoninteractiveWithoutYes(t *testing.T) {
	approved := false
	_, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, _ string, request appspkg.Request) (appspkg.Result, error) {
		if request.Action == "browser.approve" {
			approved = true
		}
		var result appspkg.Result
		if err := json.Unmarshal([]byte(`{"pairing":{"userAgent":"TestBrowser/1.0","sourceIP":"192.0.2.9","ageSeconds":90}}`), &result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}}, "app", "browser", "approve", "pc", "abcde-fghjk")
	if err == nil || approved || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("noninteractive approval was not refused: approved=%t err=%v", approved, err)
	}
}

func TestBrowserApprovalYesInspectsBeforeGranting(t *testing.T) {
	var actions []string
	out, stderr, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, _ string, request appspkg.Request) (appspkg.Result, error) {
		actions = append(actions, request.Action)
		var result appspkg.Result
		if err := json.Unmarshal([]byte(`{"pairing":{"userAgent":"TestBrowser/1.0","sourceIP":"192.0.2.9","ageSeconds":90}}`), &result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}}, "app", "browser", "approve", "pc", "abcde-fghjk", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, ",") != "browser.inspect,browser.approve" {
		t.Fatalf("approval action sequence = %v", actions)
	}
	for _, value := range []string{"TestBrowser/1.0", "192.0.2.9", "1m30s"} {
		if !strings.Contains(out+stderr, value) {
			t.Fatalf("approving owner did not see %q", value)
		}
	}
}
