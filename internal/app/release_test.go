package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// releaseOrchestrator is a recordingOrchestrator that can also run tasks.
//
// It counts applies and keeps every AppSpec it was given, which is what makes
// the veto assertable. The interesting question is never "did the status column
// say failed" — that is easy to satisfy and easy to get wrong in a way nobody
// notices. It is "was the cluster told to run the new thing", and only a fake
// that records what the cluster was told can answer it.
type releaseOrchestrator struct {
	*orchestrator.Noop

	mu      sync.Mutex
	applied []orchestrator.AppSpec
	tasks   []orchestrator.TaskSpec

	// result is what RunTask returns. Zero value succeeds.
	output    string
	succeeded bool
	runErr    error
}

func newReleaseOrch() *releaseOrchestrator {
	return &releaseOrchestrator{Noop: orchestrator.NewNoop(), succeeded: true}
}

func (r *releaseOrchestrator) ApplyApp(ctx context.Context, spec orchestrator.AppSpec) error {
	r.mu.Lock()
	r.applied = append(r.applied, spec)
	r.mu.Unlock()
	return r.Noop.ApplyApp(ctx, spec)
}

func (r *releaseOrchestrator) RunTask(
	_ context.Context, spec orchestrator.TaskSpec,
) (orchestrator.TaskResult, error) {
	r.mu.Lock()
	r.tasks = append(r.tasks, spec)
	out, ok, err := r.output, r.succeeded, r.runErr
	r.mu.Unlock()
	return orchestrator.TaskResult{Output: out, Succeeded: ok}, err
}

func (r *releaseOrchestrator) applyCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

// lastApplied is what the cluster was most recently told to run. The thing a
// failed release must not change.
func (r *releaseOrchestrator) lastApplied() (orchestrator.AppSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.applied) == 0 {
		return orchestrator.AppSpec{}, false
	}
	return r.applied[len(r.applied)-1], true
}

func (r *releaseOrchestrator) lastTask() (orchestrator.TaskSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) == 0 {
		return orchestrator.TaskSpec{}, false
	}
	return r.tasks[len(r.tasks)-1], true
}

func (r *releaseOrchestrator) fail(output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.succeeded, r.output = false, output
}

// The rest of Runner. A release uses only RunTask; these exist so the fake
// satisfies the interface the service type-asserts for, which is the thing
// under test — an orchestrator that implements only half of it is not a Runner
// and must not be treated as one.
func (r *releaseOrchestrator) EnsureSchedule(context.Context, orchestrator.ScheduleSpec) error {
	return nil
}

func (r *releaseOrchestrator) DeleteSchedule(context.Context, orchestrator.Ref) error {
	return nil
}

func (r *releaseOrchestrator) ScheduleRuns(
	context.Context, orchestrator.Ref, int,
) ([]orchestrator.RunInfo, error) {
	return nil, nil
}

var _ orchestrator.Runner = (*releaseOrchestrator)(nil)

// releaseService is testService with an orchestrator that can run tasks.
func releaseService(t *testing.T) (*Service, *releaseOrchestrator, *pgxpool.Pool) {
	t.Helper()
	s, _, pool := testService(t, Options{})
	orch := newReleaseOrch()
	s.orch = orch
	return s, orch, pool
}

// waitForDeployment polls until the deployment finishes.
//
// The release path is backgrounded — a release takes minutes in production —
// so the test has to wait for the goroutine rather than assert immediately.
func waitForDeployment(t *testing.T, s *Service, ownerID string, id uuid.UUID) Deployment {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		row, err := s.q.GetDeployment(context.Background(), dbgen.GetDeploymentParams{OwnerID: ownerID, ID: id})
		if err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		if row.FinishedAt.Valid {
			return Deployment{
				ID: row.ID, AppID: row.AppID, Image: row.Image,
				Status: row.Status, Message: row.Message,
				StartedAt: row.StartedAt,
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the deployment never finished")
	return Deployment{}
}

// latestDeployment returns an app's most recent deployment row.
func latestDeployment(t *testing.T, s *Service, ownerID string, appID uuid.UUID) Deployment {
	t.Helper()
	deps, err := s.Deployments(context.Background(), ownerID, appID, 1)
	if err != nil {
		t.Fatalf("deployments: %v", err)
	}
	if len(deps) == 0 {
		t.Fatal("no deployments")
	}
	return deps[0]
}

// releaseOf reads the release columns straight from the row.
func releaseOf(t *testing.T, s *Service, ownerID string, id uuid.UUID) (status, log string) {
	t.Helper()
	row, err := s.q.GetDeployment(context.Background(), dbgen.GetDeploymentParams{OwnerID: ownerID, ID: id})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return row.ReleaseStatus, row.ReleaseLog
}

// THE test for this stage.
//
// A release that exits non-zero must veto the deploy, and the assertion that
// matters is not the status column — it is that the cluster was never told to
// run the new thing. A test that checked only release_status would pass against
// an implementation that recorded "failed" and applied anyway, which is the
// exact bug worth catching: the deploy reports failure while the broken image
// serves traffic.
//
// So the image on the app row is moved first, the way a build moves it, and the
// assertion is that the LAST SPEC THE CLUSTER RECEIVED still names the old one.
func TestAFailedReleaseLeavesTheOldImageServing(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-veto")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:OLD", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// What the cluster is running before any of this.
	beforeSpec, ok := orch.lastApplied()
	if !ok {
		t.Fatal("create did not apply")
	}
	if beforeSpec.Image != "nginx:OLD" {
		t.Fatalf("the cluster has %q, want nginx:OLD", beforeSpec.Image)
	}
	appliesBefore := orch.applyCount()

	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	// The image the build would have produced, written the way runBuild writes
	// it — before the release runs. This is what makes the veto meaningful:
	// the row already says NEW, and only the apply would tell the cluster.
	if _, err := pool.Exec(ctx,
		`UPDATE apps SET image = 'nginx:NEW' WHERE owner_id = $1 AND id = $2`,
		ownerID, a.ID); err != nil {
		t.Fatalf("set image: %v", err)
	}

	orch.fail("migration failed: relation \"users\" already exists")

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy returned an error synchronously: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	// (a) apply was never called again.
	if got := orch.applyCount(); got != appliesBefore {
		t.Errorf("apply ran %d more time(s) after a failed release — the deploy "+
			"was not vetoed", got-appliesBefore)
	}

	// (b) the cluster still holds the OLD image. The assertion that costs.
	after, _ := orch.lastApplied()
	if after.Image != "nginx:OLD" {
		t.Errorf("the cluster was told to run %q after a failed release; "+
			"nginx:OLD must still be serving", after.Image)
	}

	// (c) the deployment says it failed.
	if dep.Status != "failed" {
		t.Errorf("deployment status = %q, want failed", dep.Status)
	}

	// (d) the release outcome and its log are recorded.
	status, log := releaseOf(t, s, ownerID, dep.ID)
	if status != ReleaseFailed {
		t.Errorf("release_status = %q, want %q", status, ReleaseFailed)
	}
	if !strings.Contains(log, "already exists") {
		t.Errorf("the release log was not kept: %q", log)
	}
}

// The other half: a release that succeeds must let the deploy through.
func TestASucceedingReleaseLetsTheDeployProceed(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-ok")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:OLD", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE apps SET image = 'nginx:NEW' WHERE owner_id = $1 AND id = $2`,
		ownerID, a.ID); err != nil {
		t.Fatalf("set image: %v", err)
	}

	orch.mu.Lock()
	orch.output, orch.succeeded = "migrated 3 tables", true
	orch.mu.Unlock()

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	after, _ := orch.lastApplied()
	if after.Image != "nginx:NEW" {
		t.Errorf("the cluster has %q after a successful release, want nginx:NEW", after.Image)
	}
	if dep.Status == "failed" {
		t.Errorf("deployment failed after a successful release: %s", dep.Message)
	}

	status, log := releaseOf(t, s, ownerID, dep.ID)
	if status != ReleaseSucceeded {
		t.Errorf("release_status = %q, want %q", status, ReleaseSucceeded)
	}
	if !strings.Contains(log, "migrated 3 tables") {
		t.Errorf("the release log was not kept: %q", log)
	}
}

// The release runs against the NEW image, which is the entire reason it sits
// after the build. Running the old one would migrate using code that predates
// the migration.
func TestTheReleaseRunsAgainstTheNewImage(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-image")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:OLD", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "sh -c 'echo hi'"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE apps SET image = 'nginx:NEW' WHERE owner_id = $1 AND id = $2`,
		ownerID, a.ID); err != nil {
		t.Fatalf("set image: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	task, ok := orch.lastTask()
	if !ok {
		t.Fatal("no release task ran")
	}
	if task.Image != "nginx:NEW" {
		t.Errorf("the release ran against %q, want nginx:NEW", task.Image)
	}
	if len(task.Command) != 3 || task.Command[0] != "sh" {
		t.Errorf("command = %v, want the parsed argv", task.Command)
	}
	if !strings.HasSuffix(task.Name, "-release") {
		t.Errorf("task name = %q, want a -release suffix", task.Name)
	}
}

// An app with no release command records "skipped", not an empty string.
//
// "The release printed nothing" and "there was no release" are different
// answers to *did my migrations run*, and this is the column that keeps them
// apart.
func TestNoReleaseCommandRecordsSkipped(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-skip")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	dep := latestDeployment(t, s, ownerID, a.ID)
	status, _ := releaseOf(t, s, ownerID, dep.ID)
	if status != ReleaseSkipped {
		t.Errorf("release_status = %q, want %q", status, ReleaseSkipped)
	}
	if _, ok := orch.lastTask(); ok {
		t.Error("a task ran for an app with no release command")
	}
}

// An IMAGE-sourced app's release command must run.
//
// Redeploy has two paths and the release hook lives in the backgrounded one,
// so an image app with a release command would take the synchronous branch and
// skip the release entirely — silently, which is the failure runRelease
// explicitly refuses to be for a missing Runner. An app deployed from a
// pre-built image with migrations to run is an ordinary thing to have.
func TestAnImageSourcedAppStillRunsItsRelease(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-imgsrc")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceImage, Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Source != SourceImage {
		t.Fatalf("source = %q, want image", a.Source)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	if _, ok := orch.lastTask(); !ok {
		t.Fatal("an image-sourced app's release command never ran")
	}
}

// And its failure vetoes the deploy just the same.
func TestAnImageSourcedAppsFailedReleaseAlsoVetoes(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-imgveto")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceImage, Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	appliesBefore := orch.applyCount()
	orch.fail("boom")

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	if got := orch.applyCount(); got != appliesBefore {
		t.Errorf("apply ran %d more time(s) despite a failed release", got-appliesBefore)
	}
	if dep.Status != "failed" {
		t.Errorf("status = %q, want failed", dep.Status)
	}
}

// The release carries the app's environment, including its sealed secrets —
// a migration needs the database URL, which is the whole point.
func TestTheReleaseCarriesTheAppsEnvironment(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-env")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
		Env: map[string]string{"LOG_LEVEL": "debug"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	task, ok := orch.lastTask()
	if !ok {
		t.Fatal("no release task ran")
	}
	if task.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("the release did not get the app's environment: %v", task.Env)
	}
}

// Volumes are never mounted, and the log says so.
//
// The constraint prevents a deadlock — RWO volumes are held by the running
// Deployment, so a task attaching them stays Pending until the deploy times out
// — and the failure it produces otherwise is a confusing "No such file or
// directory". The note is what turns that into something readable.
func TestAReleaseMountsNoVolumesAndSaysSo(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-vol")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AttachVolume(ctx, ownerID, "web", VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	task, ok := orch.lastTask()
	if !ok {
		t.Fatal("no release task ran")
	}
	if len(task.Mounts) != 0 {
		t.Errorf("the release mounted %d volume(s) — on a single-node install "+
			"that deadlocks against the running Deployment", len(task.Mounts))
	}

	_, log := releaseOf(t, s, ownerID, dep.ID)
	if !strings.Contains(log, "volumes are not mounted") {
		t.Errorf("the log does not explain the missing volume: %q", log)
	}
}

// An orchestrator that cannot run tasks must FAIL a deploy that has a release
// command, not skip it. A release command that quietly does not run is worse
// than no release command.
func TestAMissingRunnerFailsTheDeploy(t *testing.T) {
	ctx := context.Background()
	// The plain recording orchestrator does not implement Runner.
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-release-norunner")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	if dep.Status != "failed" {
		t.Errorf("status = %q, want failed — a release that cannot run must not "+
			"be silently skipped", dep.Status)
	}
	status, _ := releaseOf(t, s, ownerID, dep.ID)
	if status != ReleaseUnavailable {
		t.Errorf("release_status = %q, want %q", status, ReleaseUnavailable)
	}
}

// A command line that does not parse is refused where it is typed, not in the
// middle of a deploy that has already built an image.
func TestSetReleaseCommandRefusesAnUnparseableLine(t *testing.T) {
	ctx := context.Background()
	s, _, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-parse")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.SetReleaseCommand(ctx, ownerID, "web", "sh -c 'unclosed"); err == nil {
		t.Error("an unparseable release command was accepted")
	}

	// And clearing it is always allowed.
	if err := s.SetReleaseCommand(ctx, ownerID, "web", ""); err != nil {
		t.Errorf("clearing the release command: %v", err)
	}
}

// A task that could not be run at all — a timeout, an unschedulable pod — is a
// failure, not a pass. A deploy must not proceed on "we could not tell".
func TestAReleaseThatCouldNotRunIsAFailure(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := releaseService(t)
	ownerID := owner(t, s, pool, "owner-release-runerr")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetReleaseCommand(ctx, ownerID, "web", "./migrate"); err != nil {
		t.Fatalf("SetReleaseCommand: %v", err)
	}

	orch.mu.Lock()
	// Succeeded true AND an error: the shape a timeout produces, and the one an
	// implementation reading only Succeeded would wave through.
	orch.succeeded, orch.runErr = true, errors.New("task did not finish in time")
	orch.mu.Unlock()

	appliesBefore := orch.applyCount()
	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	dep := waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	if dep.Status != "failed" {
		t.Errorf("status = %q, want failed", dep.Status)
	}
	if got := orch.applyCount(); got != appliesBefore {
		t.Errorf("apply ran despite a release that could not be run to completion")
	}
	status, log := releaseOf(t, s, ownerID, dep.ID)
	if status != ReleaseFailed {
		t.Errorf("release_status = %q, want %q", status, ReleaseFailed)
	}
	if !strings.Contains(log, "did not finish") {
		t.Errorf("the reason was not recorded: %q", log)
	}
}
