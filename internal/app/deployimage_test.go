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
