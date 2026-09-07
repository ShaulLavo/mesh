package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
)

func noticeCallbacks() cli.UpdateNoticeCallbacks {
	return cli.UpdateNoticeCallbacks{
		Cached:  func() cli.UpdateNotice { return cli.UpdateNotice{Version: "v0.2.0"} },
		Dismiss: func(context.Context, cli.UpdateNoticeDismissal) error { return nil },
	}
}

func TestBothPickersReviewSameUpdate(t *testing.T) {
	full := newModel(pickerFixture(), pickerTestNow)
	full.configureUpdateNotice(noticeCallbacks())
	assertUpdateNotice(t, full.View().Content)
	updated, command := full.Update(runeKey('u'))
	chosen := updated.(model)
	if selection := cliSelection(chosen.selection); !selection.ReviewUpdate || selection.SessionID != "" || command == nil {
		t.Fatalf("full picker update selected attachment: %+v", selection)
	}
	input := windowFixture()
	input.UpdateNotice = noticeCallbacks()
	compact := newWindowModel(context.Background(), input, pickerTestNow)
	assertUpdateNotice(t, compact.View().Content)
	updated, command = compact.Update(runeKey('u'))
	selection := updated.(windowModel).selection
	if selection == nil || !selection.ReviewUpdate || selection.SessionID != "" || command == nil {
		t.Fatalf("compact picker update selected attachment: %+v", selection)
	}
}

func assertUpdateNotice(t *testing.T, view string) {
	t.Helper()
	plain := ansi.Strip(view)
	for _, want := range []string{"Mesh v0.2.0 is available.", "u Review update", "d Remind me tomorrow", "v Skip this version"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("notice lacks %q:\n%s", want, plain)
		}
	}
	assertFits(t, view, 80, 24)
}

func TestNoticeRefreshDoesNotDelayPickerInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	callbacks := noticeCallbacks()
	callbacks.Refresh = func(ctx context.Context) cli.UpdateNotice {
		close(entered)
		<-ctx.Done()
		return cli.UpdateNotice{}
	}
	current := newInspectingModel(ctx, pickerFixture(), nil, pickerTestNow)
	current.configureUpdateNotice(callbacks)
	command := current.refreshUpdateNotice()
	done := make(chan tea.Msg, 1)
	go func() { done <- command() }()
	<-entered
	updated, _ := current.Update(key(tea.KeyEnter))
	if updated.(model).screen != sessionScreen {
		t.Fatal("pending release lookup blocked session browsing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notice check ignored picker cancellation")
	}
}

func TestNoticeDismissalCannotHidePendingRolloutOrBeUndoneByStaleRefresh(t *testing.T) {
	var request cli.UpdateNoticeDismissal
	callbacks := noticeCallbacks()
	callbacks.Cached = func() cli.UpdateNotice {
		return cli.UpdateNotice{Version: "v0.2.0", Pending: "1 machine pending offline"}
	}
	callbacks.Dismiss = func(_ context.Context, value cli.UpdateNoticeDismissal) error { request = value; return nil }
	current := newModel(pickerFixture(), pickerTestNow)
	current.configureUpdateNotice(callbacks)
	updated, command := current.Update(runeKey('v'))
	dismissing := updated.(model)
	if dismissing.updateNotice.value.Version != "" || dismissing.updateNotice.value.Pending == "" {
		t.Fatalf("dismissal lost rollout state: %+v", dismissing.updateNotice.value)
	}
	updated, _ = dismissing.Update(command())
	dismissed := updated.(model)
	if !request.Skip || request.Version != "v0.2.0" {
		t.Fatalf("dismissed wrong version: %+v", request)
	}
	updated, _ = dismissed.Update(updateNoticeResultMsg{generation: 0, value: callbacks.Cached()})
	if updated.(model).updateNotice.value.Version != "" {
		t.Fatal("stale asynchronous refresh resurrected skipped notice")
	}
	if !strings.Contains(ansi.Strip(updated.(model).View().Content), "1 machine pending offline") {
		t.Fatal("skipped release hid unfinished rollout")
	}
}

func TestDismissalFailureRestoresNotice(t *testing.T) {
	callbacks := noticeCallbacks()
	callbacks.Dismiss = func(context.Context, cli.UpdateNoticeDismissal) error { return errors.New("read-only disk") }
	current := newModel(pickerFixture(), pickerTestNow)
	current.configureUpdateNotice(callbacks)
	updated, command := current.Update(runeKey('d'))
	dismissing := updated.(model)
	updated, _ = dismissing.Update(command())
	finished := updated.(model)
	if finished.updateNotice.value.Version != "v0.2.0" || !strings.Contains(ansi.Strip(finished.View().Content), "Could not save update reminder") {
		t.Fatalf("dismiss failure was presented as success:\n%s", finished.View().Content)
	}
}

func TestUpdateNoticeFitsSmallPickerWindows(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {40, 12}, {24, 7}, {20, 5}, {10, 3}, {4, 1}} {
		current := newModel(pickerFixture(), pickerTestNow)
		current.configureUpdateNotice(noticeCallbacks())
		current.enterSessions(0)
		updated, _ := current.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFits(t, updated.(model).View().Content, size[0], size[1])
		input := windowFixture()
		input.UpdateNotice = noticeCallbacks()
		compact := newWindowModel(context.Background(), input, pickerTestNow)
		updated, _ = compact.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFits(t, updated.(windowModel).View().Content, size[0], size[1])
	}
}

func TestNoNoticeCallbacksNeverStartsReleaseLookup(t *testing.T) {
	current := newModel(pickerFixture(), pickerTestNow)
	if current.refreshUpdateNotice() != nil {
		t.Fatal("unconfigured picker scheduled network work")
	}
	updated, command := current.Update(runeKey('u'))
	if updated.(model).selection != nil || command != nil {
		t.Fatal("absent notice unexpectedly selected update")
	}
}
