package app

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// stubBuilder answers for a build without running one.
type stubBuilder struct {
	state orchestrator.BuildState
	err   error

	built int
}

func (b *stubBuilder) Build(
	context.Context, orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.built++
	return orchestrator.BuildResult{}, errors.New("not run in this test")
}

func (b *stubBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-test" }

func (b *stubBuilder) BuildState(
	context.Context, string,
) (orchestrator.BuildState, error) {
	return b.state, b.err
}

// stubImages names an image without a registry.
type stubImages struct{}

func (stubImages) ImageFor(_ context.Context, owner, app, rev string) (string, error) {
	return "registry.test/" + owner + "-" + app + ":" + rev, nil
}
func (stubImages) Configured(context.Context) bool { return true }
func (stubImages) Insecure(context.Context) bool   { return false }
func (stubImages) DockerConfig(context.Context) ([]byte, error) {
	return []byte(`{"auths":{}}`), nil
}

// abandonedBuild writes a build that claims to be running and is not.
//
// Aged past the grace period directly in the database, because the alternative
// is a test that sleeps for two minutes to prove a timestamp comparison.
func abandonedBuild(t *testing.T, s *Service, ownerID string, a App) dbgen.Build {
	t.Helper()
	ctx := context.Background()

	deploy := s.beginDeployment(ctx, ownerID, a, "redeploy")
	row, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: deploy,
		RepoUrl: "https://example.test/x.git", RepoRef: "main",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	if err := s.q.SetBuildJob(ctx, dbgen.SetBuildJobParams{
		ID: row.ID, JobName: "build-gone",
	}); err != nil {
		t.Fatalf("SetBuildJob: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE builds SET started_at = now() - interval '1 hour' WHERE id = $1`,
		row.ID); err != nil {
		t.Fatalf("age the build: %v", err)
	}
	return row
}

// A build whose Job is gone stops claiming to run.
//
// This is the failure the reconciler exists for: the goroutine driving a build
// does not survive a restart, so without something reading the cluster the
// deployment sits on "running" for as long as the row is kept.
func TestABuildWhoseJobIsGoneIsSettled(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{} // Found: false — no such Job.
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildFailed {
		t.Errorf("build status = %q, want %q", got.Status, BuildFailed)
	}
	if got.Message == "" {
		t.Error("nothing says why the build ended")
	}

	// And the deployment it was for, which is the row somebody actually looks
	// at. A settled build under a deployment still marked running would have
	// fixed nothing.
	deps, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	for _, d := range deps {
		if d.ID == row.DeploymentID && d.Status == DeployRunning {
			t.Error("the deployment is still running after its build was settled")
		}
	}
}

// A build whose Job is still going is left alone.
//
// The reconciler runs every minute against every running build, so a version
// that could not tell "still working" from "gone" would kill each build about
// a minute in.
func TestABuildStillRunningIsNotTouched(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{Found: true}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-live")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("a running build was settled: status = %q", got.Status)
	}
}

// A build that has only just started is left alone.
//
// The row is written before the Job is created, so a reconcile landing in that
// window sees no Job. Without the grace period it would fail every build a
// moment after it began — and the reconciler runs every minute, so it would.
func TestABuildThatJustStartedIsNotSettled(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-young")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deploy := s.beginDeployment(ctx, ownerID, a, "redeploy")
	row, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: deploy,
		RepoUrl: "https://example.test/x.git", RepoRef: "main",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("a build seconds old was settled: %q — %s", got.Status, got.Message)
	}
}

// A cluster that will not answer is not evidence a build died.
//
// Failing on a read error would turn an unreachable API server into every
// in-flight deployment being marked failed, all at once, a minute later.
func TestAnUnreadableClusterSettlesNothing(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{err: errors.New("connection refused")}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-down")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds returned an error rather than skipping: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("an unreachable cluster settled a build: %q", got.Status)
	}
}

// Settling twice writes the same thing.
//
// Several replicas run this at once and all of them reach the same conclusion,
// so the second one through must not corrupt what the first wrote.
func TestSettlingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-twice")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	for i := range 2 {
		if err := s.ReconcileBuilds(ctx); err != nil {
			t.Fatalf("ReconcileBuilds pass %d: %v", i, err)
		}
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildFailed {
		t.Errorf("status = %q after two passes", got.Status)
	}
}

// A Job that finished a moment ago still belongs to the goroutine that ran it:
// that goroutine tails the log, waits for the Job to be deleted, and only then
// writes the result. A reconcile landing in between used to fail a build that
// had just succeeded.
func TestABuildThatJustFinishedIsLeftToItsOwner(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{
		Found: true, Done: true, FinishedAt: time.Now(),
	}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-fresh")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Fatalf("build status = %q — settled while its owner was still recording it",
			got.Status)
	}
}

// Two things finish a build, and the second must not overwrite the first. The
// reconciler settling a build its owner has already recorded as succeeded used
// to fail the deployment under it.
func TestSettlingDoesNotOverwriteARecordedResult(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{} // Found: false — the Job has been cleaned up.
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-raced")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	// The owner records its result after the reconciler has listed the build
	// and before it settles it.
	if _, err := s.q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: BuildSucceeded, Image: "registry.test/web:abc",
	}); err != nil {
		t.Fatalf("FinishBuild as the owner: %v", err)
	}
	s.settleBuild(ctx, row, orchestrator.BuildState{})

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildSucceeded {
		t.Errorf("build status = %q, want the owner's %q to stand", got.Status, BuildSucceeded)
	}
	deps, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	for _, d := range deps {
		if d.ID == row.DeploymentID && d.Status == DeployFailed {
			t.Error("the deployment was failed by a reconciler that had lost the race")
		}
	}

	// And the other order: a result arriving after the build was settled gets
	// no row, so its writer knows not to act on it.
	if _, err := s.q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: BuildFailed, Message: "late",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a second FinishBuild: err = %v, want pgx.ErrNoRows", err)
	}
}

// deploymentStatus reads one deployment's status straight from the table.
func deploymentStatus(t *testing.T, s *Service, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status FROM deployments WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read deployment %s: %v", id, err)
	}
	return status
}

// A deploy interrupted after its build — or one that never had a build — has
// no Job the build reconciler could ask about, so it sat on "running" until
// the next deploy happened to supersede it.
func TestADeploymentNothingIsDrivingIsFailed(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-stale-deploy")

	age := func(id uuid.UUID, by string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE deployments SET started_at = now() - $2::interval WHERE id = $1`,
			id, by); err != nil {
			t.Fatalf("age the deployment: %v", err)
		}
	}
	newApp := func(name string) App {
		t.Helper()
		a, err := s.Create(ctx, ownerID, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 80,
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return a
	}

	// Abandoned hours ago.
	stale := s.beginDeployment(ctx, ownerID, newApp("stale"), "redeploy")
	age(stale, "3 hours")

	// Started a moment ago: somebody is working on it.
	fresh := s.beginDeployment(ctx, ownerID, newApp("fresh"), "redeploy")

	// Old, but its build still claims to run. That is the build reconciler's
	// to settle, because it can ask the cluster what became of the Job.
	building := abandonedBuild(t, s, ownerID, newApp("building"))
	age(building.DeploymentID, "3 hours")

	if err := s.ReconcileDeployments(ctx); err != nil {
		t.Fatalf("ReconcileDeployments: %v", err)
	}

	if got := deploymentStatus(t, s, stale); got != DeployFailed {
		t.Errorf("the abandoned deployment is %q, want %q", got, DeployFailed)
	}
	if got := deploymentStatus(t, s, fresh); got != DeployRunning {
		t.Errorf("a deployment that had just started is %q, want it left running", got)
	}
	if got := deploymentStatus(t, s, building.DeploymentID); got != DeployRunning {
		t.Errorf("a deployment whose build is still running is %q, want it left to "+
			"the build reconciler", got)
	}
}

// The deploy being recorded often ended because its context did — the deploy
// cap, or a client that hung up — and a write made on that dead context failed,
// which is how a finished deploy stayed "running".
func TestADeploymentIsFinishedEvenWhenItsContextIsDead(t *testing.T) {
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-dead-ctx")

	a, err := s.Create(context.Background(), ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := s.beginDeployment(context.Background(), ownerID, a, "redeploy")

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	s.endDeployment(dead, ownerID, id, context.DeadlineExceeded)

	if got := deploymentStatus(t, s, id); got != DeployFailed {
		t.Fatalf("deployment status = %q, want %q", got, DeployFailed)
	}
}

// Scaling while a build runs must not retire that build's deployment. It did,
// and the build then finished into a deployment that was no longer current: its
// image was refused and never applied, with a green "scale" row left on top.
func TestScalingDoesNotSupersedeADeployInFlight(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-scale-inflight")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inFlight := s.beginDeployment(ctx, ownerID, a, "redeploy")

	scaled, err := s.Scale(ctx, ownerID, a.Name, 3)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if scaled.Replicas != 3 {
		t.Errorf("replicas = %d, want 3 — the scale itself must still take", scaled.Replicas)
	}
	if got := deploymentStatus(t, s, inFlight); got != DeployRunning {
		t.Fatalf("the deploy in flight is %q after a scale, want it still running", got)
	}
	if !s.stillCurrent(ctx, ownerID, inFlight) {
		t.Fatal("the deploy in flight is no longer current, so its image would be dropped")
	}

	// With nothing in flight a scale is still recorded as a deployment.
	s.endDeployment(ctx, ownerID, inFlight, nil)
	if _, err := s.Scale(ctx, ownerID, a.Name, 2); err != nil {
		t.Fatalf("Scale again: %v", err)
	}
	deps, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) == 0 || deps[0].Revision != "scale:2" {
		t.Errorf("newest deployment = %+v, want the scale recorded", deps)
	}
}

// Builds run one at a time, and a deploy overtaken while it waited its turn is
// not built at all. Only the newest deploy of an app can be applied, so every
// other build was up to half an hour of the node's memory spent on an image
// nobody could use.
func TestAQueuedBuildThatWasSupersededIsNotBuilt(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-build-queue")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Treated as a git app for the build path only; nothing here reaches a
	// repository.
	a.Source = SourceGit
	a.Repo = Repo{URL: "https://example.test/x.git"}

	// Another build holds the slot.
	s.buildSlot <- struct{}{}

	first := s.beginDeployment(ctx, ownerID, a, "redeploy")
	result := make(chan error, 1)
	go func() {
		_, err := s.buildIfNeeded(ctx, ownerID, a, first)
		result <- err
	}()

	select {
	case err := <-result:
		t.Fatalf("the build did not wait for the slot: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// A newer deploy arrives while the first is still queued.
	s.beginDeployment(ctx, ownerID, a, "redeploy")
	<-s.buildSlot

	select {
	case err := <-result:
		if !errors.Is(err, ErrSuperseded) {
			t.Fatalf("err = %v, want ErrSuperseded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued build never returned after the slot was freed")
	}
	if builder.built != 0 {
		t.Errorf("a superseded deploy was built anyway (%d builds)", builder.built)
	}

	// And a deploy that gives up while queued leaves the queue rather than
	// holding its place in it.
	s.buildSlot <- struct{}{}
	defer func() { <-s.buildSlot }()
	waiting, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.buildIfNeeded(waiting, ownerID, a, uuid.Nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait for the slot: err = %v, want context.Canceled", err)
	}
}
