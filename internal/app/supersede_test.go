package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// gatedBuilder holds the first build open until the test lets it finish.
//
// Time-based overlap is not overlap: sleeping for a while and hoping two
// goroutines interleave produces a test that passes for the wrong reason on a
// fast machine and flakes on a loaded one. This makes the ordering the test
// needs an actual guarantee.
type gatedBuilder struct {
	mu    sync.Mutex
	built int

	started chan struct{} // closed once the first build is genuinely under way
	release chan struct{} // closed to let that first build return
}

func newGatedBuilder() *gatedBuilder {
	return &gatedBuilder{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *gatedBuilder) Build(
	ctx context.Context, _ orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.mu.Lock()
	b.built++
	first := b.built == 1
	b.mu.Unlock()

	if first {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return orchestrator.BuildResult{}, ctx.Err()
		}
	}
	return orchestrator.BuildResult{}, nil
}

func (b *gatedBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-gated" }

func (b *gatedBuilder) BuildState(
	context.Context, string,
) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{Found: true}, nil
}

// waitForBuild waits until a deployment's build has stopped running.
//
// The overtaken goroutine has nothing to announce itself with, but its build
// row does: runBuild records the outcome before buildIfNeeded gets as far as
// the image write. This puts the assertions below just after the moment that
// used to go wrong, rather than at an arbitrary point in a sleep.
func waitForBuild(t *testing.T, s *Service, ownerID string, deployID uuid.UUID) Build {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, err := s.BuildForDeployment(context.Background(), ownerID, deployID)
		if err == nil && !b.Running() {
			return b
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the build never finished")
	return Build{}
}

// A deploy that has been overtaken stops where it is.
//
// Two deploys of one app overlap whenever a redeploy starts while an earlier
// build is still going, and Create's own deploy plus an immediate redeploy is
// the ordinary way to arrive there. Both used to finish by writing apps.image
// and applying to the cluster — in completion order rather than start order,
// so the slower-but-older one won. The app was then recorded as running an
// image its own newest deployment row did not name, which is a rollback menu
// pointing at something nobody is running, and the cluster had the older spec.
//
// beginDeployment always retired the earlier deployment. Nothing downstream
// honoured the retirement, and the retired row was flipped back to 'active' by
// the goroutine finishing under it.
func TestAnOvertakenDeployStopsWhereItIs(t *testing.T) {
	ctx := context.Background()
	b := newGatedBuilder()
	s, orch, pool := testService(t, Options{Builder: b, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-superseded")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceGit, Port: 80, Replicas: 1,
		Repo: Repo{URL: "https://example.test/x.git", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wait for that build to be running before redeploying, so the two deploys
	// genuinely overlap instead of the test racing a goroutine into existence.
	select {
	case <-b.started:
	case <-time.After(15 * time.Second):
		t.Fatal("the deploy Create started never reached its build")
	}
	overtaken := latestDeployment(t, s, ownerID, a.ID)

	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	winner := latestDeployment(t, s, ownerID, a.ID)
	if winner.ID == overtaken.ID {
		t.Fatal("Redeploy did not open a deployment of its own")
	}
	waitForDeployment(t, s, ownerID, winner.ID)

	after, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Image == PendingImage {
		t.Fatal("the redeploy recorded no image at all")
	}

	// Everything the overtaken deploy does from here is what used to go wrong.
	close(b.release)
	waitForBuild(t, s, ownerID, overtaken.ID)

	// A short settle, because the image write is the statement after the one
	// waitForBuild observed. With the bug the flip landed inside this window
	// every time; without it there is nothing left to land.
	time.Sleep(500 * time.Millisecond)

	if now, err := s.Get(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Get: %v", err)
	} else if now.Image != after.Image {
		t.Errorf("the overtaken deploy overwrote the app image with %q, want %q",
			now.Image, after.Image)
	}

	// The cluster keeps the newer spec: an apply from the overtaken deploy
	// would put the older image back after the newer one had gone out.
	if got := orch.lastAppSpec().Image; got != after.Image {
		t.Errorf("the cluster was last applied %q, want %q", got, after.Image)
	}

	// And the retirement held.
	rows, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("deployments: %v", err)
	}
	var active int
	for _, d := range rows {
		switch {
		case d.ID == overtaken.ID && d.Status != DeploySuperseded:
			t.Errorf("the overtaken deployment is %q, want %q", d.Status, DeploySuperseded)
		case d.ID == winner.ID && d.Status != DeployActive:
			t.Errorf("the winning deployment is %q, want %q", d.Status, DeployActive)
		}
		if d.Status == DeployActive {
			active++
		}
	}
	if active != 1 {
		t.Errorf("%d deployments are active, want exactly 1", active)
	}
}
