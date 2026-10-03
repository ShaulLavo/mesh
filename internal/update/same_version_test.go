package update

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

type enrollmentRemote struct {
	*fakeRemote
	denied string
}

func (remote *enrollmentRemote) Call(ctx context.Context, host Host, action string, input, output any) error {
	if host.ID == remote.denied {
		return &RemoteError{Problem: "update administrator is not enrolled on this host"}
	}
	return remote.fakeRemote.Call(ctx, host, action, input, output)
}

func sameVersionFixture(t *testing.T) (*Coordinator, *enrollmentRemote, Run) {
	t.Helper()
	c, remote, run := testCoordinator(t, 3)
	for _, index := range []int{0, 2} {
		host := run.Targets[index].Host.ID
		info := remote.info[host]
		info.Health.Build.Version = run.Release.Version
		info.Health.Build.Digest = run.Release.Artifacts[0].BinarySHA256
		remote.info[host] = info
	}
	contents, err := json.Marshal(run.Release)
	if err != nil {
		t.Fatal(err)
	}
	c.Release = release.Client{HTTPClient: &http.Client{Transport: releaseDownloadTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(contents)), Request: request}, nil
	})}}
	denied := &enrollmentRemote{fakeRemote: remote, denied: run.Targets[1].Host.ID}
	c.Remote = denied
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = c.Store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Targets[0].State != Updated || run.Targets[1].State != Failed || !run.Stopped {
		t.Fatalf("known-good host and failed enrollment not reproduced: %+v", run)
	}
	return c, denied, run
}

func TestStoppedUpdateObservesCurrentCoordinatorWithoutActivation(t *testing.T) {
	c, remote, run := sameVersionFixture(t)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := c.Store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	local := run.Targets[2]
	if local.State != Updated || local.Build == nil || local.Build.Version != run.Release.Version {
		t.Fatalf("current coordinator stayed stale after host failure: state=%s build=%+v", local.State, local.Build)
	}
	if remote.stages != 0 || len(remote.grants) != 0 || !run.Stopped || run.Targets[1].State != Failed {
		t.Fatalf("observing a stopped run authorized work or erased failure: %+v", run)
	}
}

func TestSameVersionApprovalRetriesFailedHostAfterRestart(t *testing.T) {
	c, remote, run := sameVersionFixture(t)
	remote.denied = ""
	store, err := OpenStore(filepath.Dir(filepath.Dir(c.Store.directory)))
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Coordinator{ID: c.ID, Store: store, Remote: remote, Release: c.Release, CacheDir: c.CacheDir, Download: c.Download}
	plan := Plan{Fleet: run.Fleet, Manifest: run.Release}
	approved, err := restarted.Start(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ID != run.ID || approved.Stopped || approved.Targets[1].State != Pending || approved.Targets[1].RetryToken != 1 {
		t.Fatalf("same-version approval did not resume failed host: stopped=%v state=%s token=%d id=%s", approved.Stopped, approved.Targets[1].State, approved.Targets[1].RetryToken, approved.ID)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			replayed, err := restarted.Start(context.Background(), plan)
			if err != nil || replayed.ID != run.ID || replayed.Targets[1].RetryToken != 1 {
				t.Errorf("concurrent re-approval changed retry identity: %+v %v", replayed, err)
			}
		})
	}
	wg.Wait()
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote.complete(run.Targets[1].Host.ID)
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := store.Read(run.ID)
	if err != nil || !final.Done() || final.ExitCode() != 0 || remote.stages != 1 || len(remote.grants) != 1 {
		t.Fatalf("failed host did not recover exactly once: %+v stages=%d grants=%v err=%v", final, remote.stages, remote.grants, err)
	}
}

func TestEnrollmentFailureNamesTrustCommand(t *testing.T) {
	id, key := testIdentity(t)
	actor, actorKey := testIdentity(t)
	authority := Authority{StateDir: t.TempDir(), ID: id, Key: key}
	request := Message{Action: "challenge", Actor: actor, Target: id, Nonce: "enrollment-check"}
	request.Sign(actorKey)
	_, err := callAuthority(&authority, request)
	if err == nil || !strings.Contains(err.Error(), "mesh update trust "+actor) || !strings.Contains(err.Error(), "this host") {
		t.Fatalf("enrollment failure has no actionable trust command: %v", err)
	}
}
