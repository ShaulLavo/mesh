package update

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestHostTargetLatestPreviewUsesCompatibleAuthenticatedDiscovery(t *testing.T) {
	f := newHostFixture(t)
	input := localTargetInput(t, f, "latest")
	old := f.files["latest/"+release.DiscoveryRootAssetName]
	previous := release.AssetRef{Tag: "v0.1.3", Name: release.DiscoveryRootAssetName, SHA256: discoveryDigest(old)}
	f.files[previous.Tag+"/"+previous.Name] = old
	future := release.DiscoveryRoot{Format: 1, Epoch: 2, Head: release.AssetRef{Tag: "v99.0.0", Name: release.UpdateMetadataAssetName, SHA256: strings.Repeat("f", 64)}, Previous: &previous}
	f.files["latest/"+release.DiscoveryRootAssetName] = mustHostJSON(future)
	read := f.updater.Source
	f.updater.Source = func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, error) {
		source, documents, err := read(ctx, current)
		documents = rejectFutureTargetRead(t, documents)
		return source, documents, err
	}
	plan, err := f.updater.PlanLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target.Version != "v0.1.3" || plan.Next == nil || plan.Next.Version != "v0.1.2" {
		t.Fatalf("latest escaped compatible discovery: %+v", plan)
	}
	if f.helperCalls != 0 {
		t.Fatal("latest preview mutated helper")
	}
	if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err == nil {
		t.Fatal("latest alias became a drifting durable intent")
	}
}

func rejectFutureTargetRead(t *testing.T, documents release.DocumentSource) release.DocumentSource {
	t.Helper()
	read := documents.Read
	documents.Read = func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		if ref.Tag == "v99.0.0" {
			t.Fatal("preview decoded future metadata through latest")
		}
		data, err := read(ctx, ref, kind)
		return data, hostError(err)
	}
	return documents
}

func TestHostTargetAlreadyCurrentPreviewHasNoNextAndCannotSubmit(t *testing.T) {
	f := newHostFixture(t)
	f.head(1)
	input := localTargetInput(t, f, "latest")
	for _, version := range []string{"latest", f.current.Version} {
		input.Version = version
		plan, err := f.updater.PlanLocalTarget(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Target.Version != f.current.Version || plan.Target.Binary != f.current.Digest || plan.Next != nil {
			t.Fatalf("current artifact fabricated a hop: %+v", plan)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(mustHostJSON(plan), &fields); err != nil {
			t.Fatal(err)
		}
		if _, found := fields["next"]; found {
			t.Fatal("no-hop preview serialized a next installation")
		}
		if _, err := f.updater.SubmitLocalTarget(t.Context(), input); err == nil {
			t.Fatal("non-newer preview was accepted as a reserved submission")
		}
	}
	status, err := f.store.HostTarget(t.Context())
	if err != nil || status.Revision != 0 || f.helperCalls != 0 {
		t.Fatalf("current preview mutated intent/helper: %+v %v", status, err)
	}
}
