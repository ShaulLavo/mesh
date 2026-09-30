package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/creack/pty"
	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/webauth"
)

const pairingTestCode = "abcde-fghjk"
const pairingApproveAction = "browser.approve"
const pairingYesArgument = "--" + browserYesFlag

func TestBrowserApprovalRefusesNoninteractiveWithoutYes(t *testing.T) {
	approved := false
	_, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, _ string, request appspkg.Request) (appspkg.Result, error) {
		if request.Action == pairingApproveAction {
			approved = true
		}
		var result appspkg.Result
		if err := json.Unmarshal([]byte(`{"pairing":{"userAgent":"TestBrowser/1.0","sourceIP":"192.0.2.9","ageSeconds":90}}`), &result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}}, "app", "browser", "approve", "pc", pairingTestCode)
	if err == nil || approved || !strings.Contains(err.Error(), pairingYesArgument) {
		t.Fatalf("noninteractive approval was not refused: approved=%t err=%v", approved, err)
	}
}

func TestBrowserApprovalYesInspectsBeforeGranting(t *testing.T) {
	var actions []string
	out, stderr, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, host string, request appspkg.Request) (appspkg.Result, error) {
		if host != "pc" || request.Code != pairingTestCode {
			t.Fatalf("wrong approval request: %#v", request)
		}
		actions = append(actions, request.Action)
		var result appspkg.Result
		if err := json.Unmarshal([]byte(`{"pairing":{"userAgent":"TestBrowser/1.0","sourceIP":"192.0.2.9","ageSeconds":90}}`), &result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}}, "app", "browser", "approve", "pc", pairingTestCode, pairingYesArgument)
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

func TestBrowserApprovalTTYDefaultsToNoAndRequiresExplicitYes(t *testing.T) {
	for _, test := range []struct {
		answer  string
		approve bool
	}{{"\n", false}, {"n\n", false}, {"wrong\n", false}, {"y\n", true}, {"YES\n", true}} {
		t.Run(strings.TrimSpace(test.answer), func(t *testing.T) {
			checkTTYBrowserApproval(t, test.answer, test.approve)
		})
	}
}

func TestBrowserApprovalFailsClosedWhenInspectionIsUnavailable(t *testing.T) {
	approved := false
	_, _, err := executeCommand(t, Dependencies{AppRequest: func(_ context.Context, _ string, r appspkg.Request) (appspkg.Result, error) {
		if r.Action == pairingApproveAction {
			approved = true
		}
		return appspkg.Result{}, nil
	}}, "app", "browser", "approve", "pc", pairingTestCode, pairingYesArgument)
	if err == nil || approved {
		t.Fatalf("missing inspection allowed approval: approved=%t err=%v", approved, err)
	}
}

func checkTTYBrowserApproval(t *testing.T, answer string, wantApproval bool) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close() //nolint:errcheck // test resource cleanup
	defer slave.Close()  //nolint:errcheck // test resource cleanup
	if _, err := master.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	approved := false
	app := &application{dependencies: Dependencies{AppRequest: ttyApprovalRequest(t, &approved)}}
	cmd := app.appBrowserCommand(&appOutput{sshPort: 2222}, "approve")
	var out, diagnostics bytes.Buffer
	cmd.SetIn(slave)
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"pc", pairingTestCode})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if approved != wantApproval {
		t.Fatalf("answer %q approved=%t, want %t", answer, approved, wantApproval)
	}
	text := diagnostics.String()
	if strings.Index(text, "TestBrowser") >= strings.Index(text, "[y/N]") || !strings.Contains(text, "Source IP: 192.0.2.9") {
		t.Fatal("prompt preceded browser details")
	}
}

func ttyApprovalRequest(t *testing.T, approved *bool) func(context.Context, string, appspkg.Request) (appspkg.Result, error) {
	t.Helper()
	return func(_ context.Context, _ string, r appspkg.Request) (appspkg.Result, error) {
		if r.Action == "browser.inspect" {
			return appspkg.Result{Pairing: &webauth.PairingInfo{UserAgent: "TestBrowser", SourceIP: "192.0.2.9", AgeSeconds: 90}}, nil
		}
		if r.Action != pairingApproveAction {
			t.Fatalf("unexpected action %s", r.Action)
		}
		*approved = true
		return appspkg.Result{}, nil
	}
}
