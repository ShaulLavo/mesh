package cli

import (
	"errors"
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/mcpgateway"
)

func profileSelection(alias, host, service string) mcpgateway.Selection {
	return mcpgateway.Selection{Alias: alias, HostID: host, ServiceID: service, Include: []string{}, Exclude: []string{}}
}

func TestUpsertSelectionKeepsAliasesAndServicesUnique(t *testing.T) {
	services := []mcpgateway.Selection{profileSelection("repo", "h1", "s1")}
	updated := profileSelection("repo", "h1", "s1")
	updated.Exclude = []string{"admin_*"}
	got, err := upsertSelection(services, updated)
	if err != nil || len(got) != 1 || got[0].Exclude[0] != "admin_*" {
		t.Fatalf("update in place: %v %v", got, err)
	}
	if _, err := upsertSelection(got, profileSelection("other", "h1", "s1")); !errors.Is(err, mcpgateway.ErrInvalid) {
		t.Fatalf("a second alias for one service: %v", err)
	}
	if _, err := upsertSelection(got, profileSelection("repo", "h2", "s2")); !errors.Is(err, mcpgateway.ErrInvalid) {
		t.Fatalf("one alias for two services: %v", err)
	}
	if got, err = upsertSelection(got, profileSelection("search", "h2", "s2")); err != nil || len(got) != 2 || got[1].Alias != "search" {
		t.Fatalf("append keeps profile order: %v %v", got, err)
	}
}

func TestRemoveAndRenameProfileEntries(t *testing.T) {
	f := mcpgateway.NewFile()
	f.Profiles["dev"] = mcpgateway.Profile{Services: []mcpgateway.Selection{profileSelection("a", "h", "s1"), profileSelection("b", "h", "s2")}}
	if err := renameAlias(&f, "dev", "a", "c"); err != nil || f.Profiles["dev"].Services[0].Alias != "c" {
		t.Fatalf("rename: %v %+v", err, f.Profiles["dev"])
	}
	if err := renameAlias(&f, "dev", "missing", "x"); !errors.Is(err, mcpgateway.ErrNotFound) {
		t.Fatalf("rename of a missing alias: %v", err)
	}
	if err := removeFromProfile(&f, []string{"dev", "c"}); err != nil || len(f.Profiles["dev"].Services) != 1 || f.Profiles["dev"].Services[0].Alias != "b" {
		t.Fatalf("remove entry: %v %+v", err, f.Profiles["dev"])
	}
	if err := removeFromProfile(&f, []string{"dev"}); err != nil || len(f.Profiles) != 0 {
		t.Fatalf("remove profile: %v %+v", err, f.Profiles)
	}
	if err := removeFromProfile(&f, []string{"dev"}); !errors.Is(err, mcpgateway.ErrNotFound) {
		t.Fatalf("remove a missing profile: %v", err)
	}
}

func TestParseNativeGroups(t *testing.T) {
	for value, want := range map[string][]string{"all": nil, "none": {}, "hosts,exec,hosts": {"hosts", "exec"}} {
		got, err := parseNativeGroups(value)
		if err != nil || !reflect.DeepEqual(got, want) || (want != nil && got == nil) {
			t.Fatalf("%q = %#v, %v", value, got, err)
		}
	}
	if _, err := parseNativeGroups("hosts,shell"); err == nil {
		t.Fatal("an unknown group was accepted")
	}
}
