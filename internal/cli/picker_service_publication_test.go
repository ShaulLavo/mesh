package cli

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerServicePublicationKeepsObservationToken(t *testing.T) {
	token := domainpolicy.CatalogToken{Digest: "observed-policy", Generation: 7}
	now := time.Now()
	host := HostRecord{ID: "host"}
	state := StateView{Policy: token, LastReply: now, Services: []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}, Sections: map[string]ObservedSection{protocol.TopicServices: {ReceivedAt: now}}}
	publication := pickerServicePublication{}
	publication.observe(host, state)
	rows := publication.current.Catalog.Rows
	if len(rows) != 1 || rows[0].Policy != token || !rows[0].Live || rows[0].Stale {
		t.Fatalf("received token or health changed: %+v", rows)
	}
	unreceived := StateView{Policy: domainpolicy.CatalogToken{Digest: "later-policy", Generation: 8}, LastReply: now, Sections: map[string]ObservedSection{}}
	publication.observe(host, unreceived)
	if publication.current.Catalog.Rows[0].Policy != token {
		t.Fatal("unreceived section relabeled retained rows")
	}
	state.Services = nil
	publication.observe(host, state)
	if len(publication.current.Catalog.Rows) != 0 {
		t.Fatal("received empty snapshot retained old rows")
	}
	state.Policy, state.Services = domainpolicy.CatalogToken{}, []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000"}}
	publication.observe(host, state)
	if publication.current.Catalog.Rows[0].Policy.Generation != 0 || publication.current.Catalog.Rows[0].Live {
		t.Fatal("unknown token became a current live observation")
	}
}
