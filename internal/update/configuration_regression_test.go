package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationUnchangedObservationDoesNotRepublish(t *testing.T) {
	c, actor, file := configurationFixture(t, 1)
	if _, err := c.Store.SetConfigurationGrant(t.Context(), 0, c.ID, c.ID, true); err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	if _, err := c.ApproveConfiguration(t.Context(), actor, approval); err != nil {
		t.Fatal(err)
	}
	dispatch, err := c.reserveConfiguration(t.Context())
	if err != nil || dispatch == nil {
		t.Fatal(err)
	}
	receipt, err := c.Store.PutConfiguration(t.Context(), actor, c.ID, c.Trusted, file.writer, dispatch.request)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.finishConfiguration(t.Context(), *dispatch, receipt, ConfigurationSnapshot{}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := c.Store.ConfigurationSnapshot(t.Context(), c.ID, file.writer, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.operation = "observe"
	if err = c.finishConfiguration(t.Context(), *dispatch, ConfigurationReceipt{}, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(c.Store.directory), "configuration.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.finishConfiguration(t.Context(), *dispatch, ConfigurationReceipt{}, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("identical saved snapshot replaced authority inode")
	}
}

func TestConfigurationMixedObserversKeepReloadPending(t *testing.T) {
	desired := FeedChange{URL: "https://feed.fixture/usage.json"}
	target := ConfigurationTarget{State: configurationSaved, Observers: []ConfigurationObservation{
		{Digest: (FeedValue{Present: true, URL: desired.URL}).Digest(), State: "enabled", Failing: true},
		{Digest: (FeedValue{}).Digest(), State: "enabled"},
	}}
	if got := configurationConsumerState(target, desired); got != "saved; dashboard reload pending" {
		t.Errorf("one live dashboard is on another digest but aggregate reports %q", got)
	}
}

func TestConfigurationRetryReplayRechecksSavedMembership(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	approval := configurationProposalFixture(t, c, actor)
	status, err := c.ApproveConfiguration(t.Context(), actor, approval)
	if err != nil {
		t.Fatal(err)
	}
	command := ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: status.Revision}
	status, err = c.ChangeConfiguration(t.Context(), actor, "retry", command)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := c.SavedFleet()
	if err != nil {
		t.Fatal(err)
	}
	fleet.Revision++
	c.SavedFleet = func() (Fleet, error) { return fleet, nil }
	if _, err = c.ChangeConfiguration(t.Context(), actor, "retry", command); err == nil {
		t.Fatal("retry replay acknowledged withdrawn saved membership")
	}
	cancel := ConfigurationCommand{ID: mustAutomaticID(t), ChangeID: approval.Proposal.ID, ExpectedRevision: status.Revision, Retire: true}
	status, err = c.ChangeConfiguration(t.Context(), actor, configurationCancel, cancel)
	if err != nil || !status.Change.Retired {
		t.Fatalf("local retirement after changed membership: %+v %v", status, err)
	}
}

func TestConfigurationObservationAdmissionDoesNotRepublish(t *testing.T) {
	c, actor, _ := configurationFixture(t, 1)
	approval := configurationProposalFixture(t, c, actor)
	if _, err := c.ApproveConfiguration(t.Context(), actor, approval); err != nil {
		t.Fatal(err)
	}
	record, err := c.Store.readConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	record.Current.Cancelled = true
	record.Current.Targets[0].Issued = true
	record.Current.Targets[0].DispatchID = mustAutomaticID(t)
	record.Current.Targets[0].Attempt = 1
	if err = c.Store.writeConfiguration(record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(c.Store.directory), "configuration.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.reserveConfiguration(t.Context())
	if err != nil || first == nil || first.operation != "observe" {
		t.Fatalf("observation admission: %+v %v", first, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("read-only observation admission replaced authority")
	}
	next, err := c.reserveConfiguration(t.Context())
	if err != nil || next != nil {
		t.Fatalf("observation was not paced in memory: %+v %v", next, err)
	}

}
