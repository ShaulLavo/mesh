package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/usagefeed"
)

const (
	configurationPending      = "pending"
	configurationIssued       = "issued"
	configurationUncertain    = "uncertain"
	configurationSaved        = "saved"
	configurationConflict     = "conflict"
	configurationRejected     = "rejected"
	configurationCancelled    = "cancelled"
	configurationPrepared     = "prepared"
	configurationCancel       = "cancel"
	configurationRetry        = "retry"
	configurationPutOperation = "put"
)

const MaximumConfigurationProposal = 64 << 10

type FeedValue struct {
	Present bool   `json:"present"`
	URL     string `json:"url"`
}

func (v FeedValue) Digest() string { return configurationDigest(v) }

// FeedFile is held only inside the existing CLI configuration writer lock.
type FeedFile interface {
	Value() FeedValue
	Save(FeedValue) error
	Settle() error
}
type ConfigurationWriter func(context.Context, func(FeedFile) error) error

type ConfigurationGrant struct {
	Schema        int    `json:"schema"`
	Revision      uint64 `json:"revision"`
	LocalHostID   string `json:"localHostId"`
	CoordinatorID string `json:"coordinatorId"`
	UsageFeed     bool   `json:"usageFeed"`
}
type ComponentExpectation struct {
	Revision uint64    `json:"revision"`
	Value    FeedValue `json:"value"`
	Digest   string    `json:"digest"`
}
type ConfigurationSnapshot struct {
	HostID    string                     `json:"hostId"`
	Grant     ConfigurationGrant         `json:"grant"`
	Feed      ComponentExpectation       `json:"feed"`
	Receipt   *ConfigurationReceipt      `json:"receipt,omitempty"`
	Observers []ConfigurationObservation `json:"observers"`
}
type FeedChange struct {
	URL string `json:"url"`
}
type ConfigurationSelection struct {
	Targets   []string   `json:"targets"`
	UsageFeed FeedChange `json:"usageFeed"`
}
type RecipientConfiguration struct {
	Host          Host                 `json:"host"`
	GrantRevision uint64               `json:"grantRevision"`
	Expected      ComponentExpectation `json:"expected"`
	Feed          FeedChange           `json:"usageFeed"`
}
type ConfigurationProposal struct {
	ID               string                   `json:"id"`
	CoordinatorID    string                   `json:"coordinatorId"`
	OwnerID          string                   `json:"ownerId"`
	ExpectedRevision uint64                   `json:"expectedRevision"`
	Fleet            Fleet                    `json:"fleet"`
	Membership       string                   `json:"membership"`
	Recipients       []RecipientConfiguration `json:"recipients"`
	Digest           string                   `json:"digest"`
}
type ConfigurationApproval struct {
	CommandID string                `json:"commandId"`
	Proposal  ConfigurationProposal `json:"proposal"`
}
type ConfigurationCommand struct {
	ID               string `json:"id"`
	ChangeID         string `json:"changeId"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	Retire           bool   `json:"retire,omitempty"`
}
type ConfigurationTarget struct {
	ObservedFeed *ComponentExpectation      `json:"observedFeed,omitempty"`
	Consumer     string                     `json:"consumerState"`
	HostID       string                     `json:"hostId"`
	State        string                     `json:"state"`
	DispatchID   string                     `json:"dispatchId,omitempty"`
	Attempt      uint64                     `json:"attempt"`
	Issued       bool                       `json:"issued"`
	RetryAt      time.Time                  `json:"retryAt,omitempty"`
	Problem      string                     `json:"problem,omitempty"`
	Receipt      *ConfigurationReceipt      `json:"receipt,omitempty"`
	Observers    []ConfigurationObservation `json:"observers"`
}
type ConfigurationChange struct {
	Proposal  ConfigurationProposal `json:"proposal"`
	Attempt   uint64                `json:"attempt"`
	Cancelled bool                  `json:"cancelled"`
	Retired   bool                  `json:"retired"`
	Blocked   string                `json:"blocked,omitempty"`
	Targets   []ConfigurationTarget `json:"targets"`
}
type ConfigurationRecord struct {
	Schema       int                     `json:"schema"`
	Revision     uint64                  `json:"revision"`
	Previews     []ConfigurationProposal `json:"previews,omitempty"`
	Current      *ConfigurationChange    `json:"current,omitempty"`
	LastMutation *MutationReceipt        `json:"lastMutation,omitempty"`
}
type ConfigurationStatus struct {
	Revision uint64               `json:"revision"`
	Change   *ConfigurationChange `json:"change"`
}
type ConfigurationPut struct {
	ChangeID       string                 `json:"changeId"`
	ProposalDigest string                 `json:"proposalDigest"`
	Membership     string                 `json:"membership"`
	CoordinatorID  string                 `json:"coordinatorId"`
	Recipient      RecipientConfiguration `json:"recipient"`
}
type ConfigurationReceipt struct {
	Schema         int                  `json:"schema"`
	ChangeID       string               `json:"changeId"`
	ProposalDigest string               `json:"proposalDigest"`
	CoordinatorID  string               `json:"coordinatorId"`
	HostID         string               `json:"hostId"`
	GrantRevision  uint64               `json:"grantRevision"`
	Membership     string               `json:"membership"`
	Expected       ComponentExpectation `json:"expected"`
	Desired        FeedValue            `json:"desired"`
	Revision       uint64               `json:"revision"`
	State          string               `json:"state"`
	Problem        string               `json:"problem,omitempty"`
}

func configurationDigest(value any) string {
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func (p ConfigurationProposal) approvalDigest() string { p.Digest = ""; return configurationDigest(p) }
func DecodeConfiguration(data []byte, value any) error {
	if len(data) == 0 || len(data) > MaximumConfigurationProposal {
		return problem("configuration.invalid", "configuration input exceeds 64 KiB or is empty")
	}
	return decodeAuthority(data, value)
}
func ValidateDeliveredFeed(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 2048 {
		return problem("configuration.invalid", "usage feed URL exceeds 2048 bytes")
	}
	if err := usagefeed.ValidateURL(value); err != nil {
		return problem("configuration.invalid", err.Error())
	}
	parsed, _ := url.Parse(value)
	_, _, private := domainpolicy.Label(parsed.Hostname(), false)
	if parsed.Scheme != "https" || !private || parsed.Port() != "" && parsed.Port() != "443" {
		return problem("configuration.invalid", "delivered usage feed needs a private HTTPS service hostname in the active domain policy")
	}
	return nil
}
func validateFeedValue(v FeedValue) error {
	if !v.Present && v.URL != "" {
		return errors.New("configuration: absent feed has a URL")
	}
	if v.URL != "" {
		if err := usagefeed.ValidateURL(v.URL); err != nil {
			return fmt.Errorf("configuration feed: %w", err)
		}
	}
	return nil
}
func validateConfigurationProposal(p ConfigurationProposal) error {
	if !ValidRunID(p.ID) || p.Digest != p.approvalDigest() || p.Membership != p.Fleet.Digest() {
		return errors.New("configuration: proposal identity or digest differs")
	}
	if _, err := PublicKey(p.OwnerID); err != nil {
		return err
	}
	if _, err := PublicKey(p.CoordinatorID); err != nil {
		return err
	}
	if err := p.Fleet.Validate(); err != nil {
		return err
	}
	if len(p.Recipients) == 0 || len(p.Recipients) > MaximumHosts {
		return errors.New("configuration: explicit recipients required")
	}
	seen := make(map[string]bool)
	for _, recipient := range p.Recipients {
		if seen[recipient.Host.ID] {
			return errors.New("configuration: duplicate recipient")
		}
		seen[recipient.Host.ID] = true
		if err := validateConfigurationRecipient(p.Fleet, recipient); err != nil {
			return err
		}
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > MaximumConfigurationProposal-1024 {
		return errors.New("configuration: proposal exceeds 64 KiB")
	}
	return nil
}
func validateConfigurationRecipient(f Fleet, recipient RecipientConfiguration) error {
	index := slices.IndexFunc(f.Members, func(h Host) bool { return h.ID == recipient.Host.ID })
	if index < 0 || configurationDigest(f.Members[index]) != configurationDigest(recipient.Host) {
		return errors.New("configuration: recipient differs from saved membership")
	}
	if recipient.GrantRevision == 0 || recipient.Expected.Digest != recipient.Expected.Value.Digest() {
		return errors.New("configuration: invalid recipient expectation")
	}
	if err := validateFeedValue(recipient.Expected.Value); err != nil {
		return err
	}
	return ValidateDeliveredFeed(recipient.Feed.URL)
}
func configurationAction(action string) bool { return strings.HasPrefix(action, "configuration.") }
func (c ConfigurationChange) settled() bool {
	for _, target := range c.Targets {
		if target.Issued || !slices.Contains([]string{configurationSaved, configurationConflict, configurationRejected, configurationCancelled}, target.State) {
			return false
		}
	}
	return true
}
func (c *Coordinator) configurationScope(actor VerifiedActor) (Fleet, error) {
	if c.Trusted == nil || !c.Trusted(actor.id) {
		return Fleet{}, problem("configuration.denied", "configuration approver is no longer an update administrator")
	}
	if c.SavedFleet == nil {
		return Fleet{}, errors.New("configuration: saved fleet unavailable")
	}
	fleet, err := c.SavedFleet()
	if err != nil {
		return Fleet{}, fmt.Errorf("configuration: saved fleet: %w", err)
	}
	return fleet, fleet.Validate()
}

func (f *FeedChange) UnmarshalJSON(data []byte) error {
	var input struct {
		URL *string `json:"url"`
	}
	if err := decodeAuthority(data, &input); err != nil {
		return err
	}
	if input.URL == nil {
		return problem("configuration.invalid", "explicit usageFeed.url string required; use an empty string to disable")
	}
	f.URL = *input.URL
	return nil
}
func (s *ConfigurationSelection) UnmarshalJSON(data []byte) error {
	var input struct {
		Targets   []string    `json:"targets"`
		UsageFeed *FeedChange `json:"usageFeed"`
	}
	if err := decodeAuthority(data, &input); err != nil {
		return err
	}
	if input.UsageFeed == nil {
		return problem("configuration.invalid", "explicit usageFeed selection required")
	}
	s.Targets, s.UsageFeed = input.Targets, *input.UsageFeed
	return nil
}
func (r *RecipientConfiguration) UnmarshalJSON(data []byte) error {
	var input struct {
		Host          Host                  `json:"host"`
		GrantRevision uint64                `json:"grantRevision"`
		Expected      *ComponentExpectation `json:"expected"`
		Feed          *FeedChange           `json:"usageFeed"`
	}
	if err := decodeAuthority(data, &input); err != nil {
		return err
	}
	if input.Expected == nil || input.Feed == nil {
		return problem("configuration.invalid", "selected usage feed and exact expectation required")
	}
	*r = RecipientConfiguration{input.Host, input.GrantRevision, *input.Expected, *input.Feed}
	return nil
}
