package update

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func TestConfigurationDomainConflictSettlesIndependentFeed(t *testing.T) {
	c, actor, feed := configurationFixture(t, 1)
	if _, err := c.Store.SetConfigurationGrant(t.Context(), 0, actor.id, actor.id, true, true); err != nil {
		t.Fatal(err)
	}
	approval := configurationProposalFixture(t, c, actor)
	before := DomainValue{Present: true, Policy: domainpolicy.Current()}
	domain := &configurationDomainFixture{value: before, fail: errors.New("directory barrier after rename")}
	recipient := approval.Proposal.Recipients[0]
	recipient.DomainExpected = &DomainExpectation{Value: before, Digest: before.Digest()}
	recipient.Domain = &DomainChange{Primary: "new.test", Aliases: before.Policy.Domains()}
	request := ConfigurationPut{mustAutomaticID(t), strings.Repeat("d", 64), approval.Proposal.Membership, c.ID, recipient}
	if _, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer); !errors.Is(err, domain.fail) {
		t.Fatal(err)
	}
	domain.fail = nil
	domain.value = DomainValue{Present: true, Policy: domainpolicy.Policy{Primary: "third.test", Aliases: append([]string{domain.value.Policy.Primary}, before.Policy.Domains()...), LegacyCertificateDomain: before.Policy.LegacyCertificateDomain}}
	feedFailure := errors.New("feed writer unavailable")
	blocked, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted,
		func(context.Context, func(FeedFile) error) error { return feedFailure }, request, domain.writer)
	if !errors.Is(err, feedFailure) || blocked.State != configurationPrepared || blocked.FeedState != configurationPrepared || blocked.Domain.State != configurationConflict {
		t.Fatalf("aggregate became terminal with an unresolved feed: %+v, %v", blocked, err)
	}
	receipt, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer)
	if err != nil || receipt.State != configurationConflict {
		t.Fatal(receipt, err)
	}
	if receipt.FeedState != configurationSaved || feed.writes != 1 || domain.value.Policy.Primary != "third.test" {
		t.Fatalf("independent feed did not settle while retaining the domain edit: %+v, writes=%d, policy=%+v", receipt, feed.writes, domain.value)
	}
	domain.value = receipt.Domain.Desired
	replayed, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, request, domain.writer)
	if err != nil || replayed.Domain.State != configurationConflict || replayed.State != configurationConflict || feed.writes != 1 {
		t.Fatalf("terminal domain conflict resumed on replay: %+v, writes=%d, error=%v", replayed, feed.writes, err)
	}
	snapshot, err := c.Store.ConfigurationSnapshot(t.Context(), actor.id, feed.writer, nil, domain.writer)
	if err != nil {
		t.Fatal(err)
	}
	fresh := request
	fresh.ChangeID = mustAutomaticID(t)
	fresh.ProposalDigest = strings.Repeat("e", 64)
	fresh.Recipient.Domain, fresh.Recipient.DomainExpected = nil, nil
	fresh.Recipient.Expected = &snapshot.Feed
	next, err := c.Store.PutConfiguration(t.Context(), actor, actor.id, c.Trusted, feed.writer, fresh, domain.writer)
	t.Logf("new approved feed-only delivery: state=%s error=%v", next.State, err)
	if err != nil {
		t.Errorf("terminal domain conflict left an unreachable prepared feed intention: %v", err)
	}
}
