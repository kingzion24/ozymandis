package app

import (
	"context"
	"testing"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// okBuilder is a build that works, which stubBuilder deliberately is not.
type okBuilder struct{ built int }

func (b *okBuilder) Build(
	context.Context, orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.built++
	return orchestrator.BuildResult{}, nil
}

func (b *okBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-ok" }

func (b *okBuilder) BuildState(
	context.Context, string,
) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{Found: true}, nil
}

// A deployment names the image it shipped, not the one it replaced.
//
// beginDeployment runs before the build and can only record the image the app
// has at that moment — for a git app, whatever the previous deploy produced.
// Nothing corrected it afterwards, so every row in a git app's history named
// its predecessor's image and the newest image appeared in no row at all. The
// list reads as a rollback menu, which made it a menu of wrong answers.
func TestADeploymentRecordsTheImageItBuilt(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-deploy-image")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceGit, Port: 80, Replicas: 1,
		Repo: Repo{URL: "https://example.test/x.git", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Image != PendingImage {
		t.Fatalf("a git app starts at %q, not %q", PendingImage, a.Image)
	}

	// Create deploys on its own, so this app already has a build in flight.
	// Let it land before starting another: buildIfNeeded writes apps.image
	// when its build finishes, two overlapping deploys therefore write it in
	// completion order rather than start order, and the app is left recorded
	// as running whichever finished last. That is worth knowing and is not
	// what this test is about — asserting on apps.image while a second build
	// is still running only measures which goroutine won.
	waitForDeployment(t, s, ownerID, latestDeployment(t, s, ownerID, a.ID).ID)

	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	deployed := latestDeployment(t, s, ownerID, a.ID)
	waitForDeployment(t, s, ownerID, deployed.ID)

	built, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if built.Image == PendingImage {
		t.Fatal("the build never recorded an image on the app")
	}

	row := latestDeployment(t, s, ownerID, a.ID)
	if row.Image != built.Image {
		t.Errorf("the deployment says it shipped %q; the app runs %q",
			row.Image, built.Image)
	}
}

// The same row, when there was nothing to build.
//
// An image-sourced app's deployment is correct without help, and the fix must
// not reach in and change what it says.
func TestADeploymentOfAnImageAppKeepsItsImage(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-deploy-image-plain")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "cache", Image: "redis:7-alpine", Port: 6379, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	if row := latestDeployment(t, s, ownerID, a.ID); row.Image != "redis:7-alpine" {
		t.Errorf("deployment image = %q, want %q", row.Image, "redis:7-alpine")
	}
}

// The source column named what the deploy was built from, and always said
// "image".
//
// It was derived from the revision string, matching a "git:" prefix that no
// code ever writes — beginDeployment writes "redeploy" or "scale:N", and
// Create writes "initial". So the Git branch was unreachable and every row in
// every list, for every app, reported the same word.
func TestADeploymentNamesWhatTheAppIsBuiltFrom(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-deploy-source")

	git, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceGit, Port: 80, Replicas: 1,
		Repo: Repo{URL: "https://example.test/x.git", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create git app: %v", err)
	}
	image, err := s.Create(ctx, ownerID, CreateInput{
		Name: "cache", Image: "redis:7-alpine", Port: 6379, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Create image app: %v", err)
	}

	if got := latestDeployment(t, s, ownerID, git.ID).Source; got != string(SourceGit) {
		t.Errorf("git app deployment source = %q, want %q", got, SourceGit)
	}
	if got := latestDeployment(t, s, ownerID, image.ID).Source; got != string(SourceImage) {
		t.Errorf("image app deployment source = %q, want %q", got, SourceImage)
	}
}

// And the same answer in the cross-app feed, which reads through a different
// query. Two lists disagreeing about one deployment is its own bug.
func TestRecentActivityNamesTheSameSource(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-activity-source")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceGit, Port: 80, Replicas: 1,
		Repo: Repo{URL: "https://example.test/x.git", Branch: "main"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	feed, err := s.RecentActivity(ctx, ownerID, 10)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(feed) == 0 {
		t.Fatal("the feed is empty")
	}
	if feed[0].Source != string(SourceGit) {
		t.Errorf("activity source = %q, want %q", feed[0].Source, SourceGit)
	}
}
