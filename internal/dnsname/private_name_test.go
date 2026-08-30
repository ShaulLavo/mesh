package dnsname

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificatePrivateNameIsProfileBoundAndSignatureCovered(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	privateBundle := testBundle(t, 1, now, now.Add(24*time.Hour))

	signed, err := SignBundle(privateBundle, targetID, ProfilePrivateOrigin, EnvironmentLive, "pc.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	tampered := cloneSignedBundle(signed)
	tampered.PrivateName = "other.mesh.shaulavo.dev"
	if _, err := VerifySignedBundle(tampered, targetID, signerID, now); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("private-name tamper error = %v", err)
	}
	if _, err := SignBundle(privateBundle, targetID, ProfilePrivateOrigin, EnvironmentLive, "nested.pc.mesh.shaulavo.dev", signer); err == nil {
		t.Fatal("nested private name was signed")
	}

	certificatePEM, keyPEM := testCertificate(t, 2, PublicWildcardName, now.Add(-time.Hour), now.Add(24*time.Hour))
	publicBundle, err := ValidateBundle(certificatePEM, keyPEM, PublicWildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignBundle(publicBundle, targetID, ProfilePublicEdge, EnvironmentLive, "pc.mesh.shaulavo.dev", signer); err == nil || !strings.Contains(err.Error(), "must not") {
		t.Fatalf("public private-name error = %v", err)
	}
}

func TestPrivateNamePersistsOnlyForLiveAndIsStableAcrossRestart(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	root := t.TempDir()
	installer, names := privateInstallerForTest(t, root, now, targetID, signerID)

	staging := testBundle(t, 10, now, now.Add(48*time.Hour))
	stagingSigned, err := SignBundle(staging, targetID, ProfilePrivateOrigin, EnvironmentStaging, "pc.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installer.Install(stagingSigned); err != nil || !changed {
		t.Fatalf("staging install changed = %v, error = %v", changed, err)
	}
	names.MarkIngressReady()
	if got := names.Current(); got != "" {
		t.Fatalf("staging private name = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "live", privateNameFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging created live private name: %v", err)
	}

	live := testBundle(t, 20, now, now.Add(72*time.Hour))
	liveSigned, err := SignBundle(live, targetID, ProfilePrivateOrigin, EnvironmentLive, "pc.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installer.Install(liveSigned); err != nil || !changed {
		t.Fatalf("live install changed = %v, error = %v", changed, err)
	}
	if got := names.Current(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("live private name = %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "live", privateNameFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private-name file = %#v, %v", info, err)
	}

	_, restartedNames := privateInstallerForTest(t, root, now, targetID, signerID)
	if got := restartedNames.Current(); got != "" {
		t.Fatalf("name exposed before ingress readiness = %q", got)
	}
	restartedNames.MarkIngressReady()
	if got := restartedNames.Current(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("restarted private name = %q", got)
	}

	restartedInstaller, restartedNames := privateInstallerForTest(t, root, now, targetID, signerID)
	restartedNames.MarkIngressReady()
	renameSigned, err := SignBundle(live, targetID, ProfilePrivateOrigin, EnvironmentLive, "other.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restartedInstaller.Install(renameSigned); err == nil || !strings.Contains(err.Error(), "already pinned") {
		t.Fatalf("private-name replay rename error = %v", err)
	}
	if got := restartedNames.Current(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("name after rejected rename = %q", got)
	}

	rotated := testBundle(t, 30, now, now.Add(96*time.Hour))
	emptySigned, err := SignBundle(rotated, targetID, ProfilePrivateOrigin, EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := restartedInstaller.Install(emptySigned); err != nil || !changed {
		t.Fatalf("empty-name rotation changed = %v, error = %v", changed, err)
	}
	if got := restartedNames.Current(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("empty rotation did not preserve name: %q", got)
	}
}

func TestEmptyLiveRenewalRepublishesNameSuppressedByExpiredCertificate(t *testing.T) {
	initial := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	signerID, signer := testEd25519Identity(t)
	targetID, _ := testEd25519Identity(t)
	root := t.TempDir()
	installer, names := privateInstallerForTest(t, root, initial, targetID, signerID)
	names.MarkIngressReady()
	short := testBundle(t, 40, initial, initial.Add(time.Hour))
	signed, err := SignBundle(short, targetID, ProfilePrivateOrigin, EnvironmentLive, "pc.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installer.Install(signed); err != nil {
		t.Fatal(err)
	}

	later := initial.Add(2 * time.Hour)
	restarted, restartedNames := privateInstallerForTest(t, root, later, targetID, signerID)
	restartedNames.MarkIngressReady()
	if got := restartedNames.Current(); got != "" {
		t.Fatalf("expired certificate exposed private name %q", got)
	}
	renewed := testBundle(t, 41, later, later.Add(72*time.Hour))
	emptySigned, err := SignBundle(renewed, targetID, ProfilePrivateOrigin, EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Install(emptySigned); err != nil {
		t.Fatal(err)
	}
	if got := restartedNames.Current(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("renewal did not republish persisted name: %q", got)
	}
}

func privateInstallerForTest(t *testing.T, root string, now time.Time, targetID, signerID string) (*Installer, *PrivateNameSource) {
	t.Helper()
	liveStore, err := NewBundleStore(filepath.Join(root, "live"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	liveStore.now = func() time.Time { return now }
	stagingStore, err := NewBundleStore(filepath.Join(root, "staging"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	stagingStore.now = func() time.Time { return now }
	liveSource, err := NewCertificateSource(liveStore)
	if err != nil {
		t.Fatal(err)
	}
	names, err := NewPrivateNameSource(liveStore)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := NewInstaller(InstallerConfig{
		Profile: ProfilePrivateOrigin, LiveSource: liveSource, StagingStore: stagingStore, PrivateName: names,
		TargetID: targetID, SignerID: signerID, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return installer, names
}
