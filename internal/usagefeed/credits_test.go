package usagefeed

import (
	"strings"
	"testing"
)

func TestOptionalCreditsContract(t *testing.T) {
	good := fixture(t)
	for _, value := range []string{`{"balance":62500,"unlimited":false}`, `{"balance":0,"unlimited":true}`, `{"balance":0,"unlimited":false}`, `{"balance":12.5,"unlimited":false}`} {
		body := strings.Replace(good, `"plan": "large",`, `"plan": "large", "credits": `+value+`,`, 1)
		if _, err := decode([]byte(body)); err != nil {
			t.Errorf("valid optional credits %s rejected: %v", value, err)
		}
	}
	for _, value := range []string{`{}`, `{"balance":null,"unlimited":false}`, `{"balance":0,"unlimited":null}`, `{"balance":0}`, `{"unlimited":false}`, `{"balance":-1,"unlimited":false}`, `{"balance":1e999,"unlimited":false}`, `{"balance":2,"unlimited":false,"secret":"bad"}`, `{"balance":"2","unlimited":false}`} {
		body := strings.Replace(good, `"plan": "large",`, `"plan": "large", "credits": `+value+`,`, 1)
		if _, err := decode([]byte(body)); err == nil {
			t.Errorf("invalid credits accepted: %s", value)
		}
	}
	if _, err := decode([]byte(good)); err != nil {
		t.Fatal("absent credits broke v1", err)
	}
}

func TestHistoricalAndFutureWindowSources(t *testing.T) {
	for _, source := range []string{"reset-order", "future-provider-cache"} {
		body := strings.Replace(fixture(t), `"source": "passive-header"`, `"source": "`+source+`"`, 1)
		snapshot, err := decode([]byte(body))
		if err != nil {
			t.Errorf("window source %s rejected whole feed: %v", source, err)
			continue
		}
		window := snapshot.Accounts[0].Windows[0]
		if *window.UsedPercent != 42 || window.LastSeenAt == nil || window.Source != source {
			t.Fatal("historic values changed", window)
		}
	}
	for _, source := range []string{"", `bad\u001bsource`} {
		body := strings.Replace(fixture(t), `"source": "passive-header"`, `"source": "`+source+`"`, 1)
		if _, err := decode([]byte(body)); err == nil {
			t.Errorf("invalid source accepted: %q", source)
		}
	}
}

func TestCreditsSnapshotsAreDetached(t *testing.T) {
	body := strings.Replace(fixture(t), `"plan": "large",`, `"plan": "large", "credits": {"balance":62500,"unlimited":false},`, 1)
	feed, _, _ := newTestFetcher(t, body)
	result := feed.Refresh(t.Context())
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Snapshot.Accounts[0].Credits == nil {
		t.Fatal("credits lost")
	}
	result.Snapshot.Accounts[0].Credits.Balance = 0
	result.Snapshot.Accounts[0].Credits.Unlimited = true
	credits := feedSnapshot(feed).Snapshot.Accounts[0].Credits
	if credits.Balance != 62500 || credits.Unlimited {
		t.Fatal("consumer mutated retained credits", credits)
	}
}
