package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestSlugifyBuildsAnAddressFromAName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Billing", "billing"},
		{"  Billing   Service ", "billing-service"},
		{"Café & Co.", "caf-co"},
		{"---", ""},
		{"...", ""},
	} {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A name with nothing to build an address from is refused rather than given a
// generated one. A URL with no relationship to what somebody typed is a URL
// they will not recognise as theirs.
func TestAProjectNeedsSomethingToAddressItBy(t *testing.T) {
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-name")

	if _, err := s.CreateProject(context.Background(), ownerID, "!!!"); err == nil {
		t.Fatal("a project named entirely of punctuation was accepted")
	}
}

// The default project is created on demand and adopts anything unassigned.
//
// This is what lets an install that predates projects open the page and see its
// apps, rather than an empty canvas with no way to explain where they went.
func TestTheDefaultProjectAdoptsAppsThatHaveNone(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-adopt")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "orphan", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Written straight to the column, because Create has no way to make an app
	// that predates the projects table — which is precisely the case at issue.
	if _, err := pool.Exec(ctx,
		`UPDATE apps SET project_id = NULL WHERE owner_id = $1`, ownerID); err != nil {
		t.Fatalf("unassign: %v", err)
	}

	projects, err := s.Projects(ctx, ownerID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Slug != DefaultProjectSlug {
		t.Fatalf("projects = %+v, want one default project", projects)
	}
	if projects[0].Apps != 1 {
		t.Fatalf("default project holds %d apps, want the orphan", projects[0].Apps)
	}

	apps, err := s.ListInProject(ctx, ownerID, projects[0].ID)
	if err != nil {
		t.Fatalf("list apps in project: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "orphan" {
		t.Fatalf("apps in default project = %+v, want the orphan", apps)
	}
}

// A position survives the round trip, and clearing gives it back to the layout.
//
// The distinction that matters is nil versus zero: (0,0) is a corner somebody
// can drag a card to, and treating it as "never moved" would re-lay-out a card
// that was deliberately pinned there.
func TestAPositionIsStoredAndCanBeGivenBack(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-pos")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	project, err := s.DefaultProject(ctx, ownerID)
	if err != nil {
		t.Fatalf("default project: %v", err)
	}

	if err := s.SetPosition(ctx, ownerID, "web", 0, 0); err != nil {
		t.Fatalf("set position: %v", err)
	}
	a, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.X == nil || a.Y == nil {
		t.Fatal("a card pinned to the origin came back as never having been moved")
	}
	if *a.X != 0 || *a.Y != 0 {
		t.Fatalf("position = (%d,%d), want (0,0)", *a.X, *a.Y)
	}

	if err := s.ClearPositions(ctx, ownerID, project.ID); err != nil {
		t.Fatalf("clear positions: %v", err)
	}
	if a, err = s.Get(ctx, ownerID, "web"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.X != nil || a.Y != nil {
		t.Fatalf("position survived a clear: (%v,%v)", a.X, a.Y)
	}
}

// Positions are scoped by owner, like everything else.
func TestAnotherTeamCannotMoveYourCards(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	mine := owner(t, s, pool, "owner-project-mine")
	theirs := owner(t, s, pool, "owner-project-theirs")

	if _, err := s.Create(ctx, mine, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	if err := s.SetPosition(ctx, theirs, "web", 10, 20); err == nil {
		t.Fatal("another team moved a card on a canvas that is not theirs")
	}
}

// Moving an app is what makes projects usable on an install that already has
// apps. Everything created before somebody organised anything is in the default
// project, and without this there is no way to take it out of there.
func TestAnAppCanBeMovedToAnotherProject(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-move")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	billing, err := s.CreateProject(ctx, ownerID, "Billing")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Dragged somewhere on the canvas it is leaving. The coordinates mean
	// nothing on the new one, so the move has to forget them — otherwise the
	// card arrives wherever the old arrangement put it, over another app or
	// outside the visible area.
	if err := s.SetPosition(ctx, ownerID, "web", 640, 480); err != nil {
		t.Fatalf("set position: %v", err)
	}

	if err := s.MoveApp(ctx, ownerID, "web", billing.Slug); err != nil {
		t.Fatalf("MoveApp: %v", err)
	}

	moved, err := s.ListInProject(ctx, ownerID, billing.ID)
	if err != nil {
		t.Fatalf("list apps in project: %v", err)
	}
	if len(moved) != 1 || moved[0].Name != "web" {
		t.Fatalf("apps in billing = %+v, want web", moved)
	}
	if moved[0].X != nil || moved[0].Y != nil {
		t.Errorf("position = (%v, %v), want none: the old canvas's coordinates "+
			"do not describe a place on this one",
			moved[0].X, moved[0].Y)
	}

	def, err := s.DefaultProject(ctx, ownerID)
	if err != nil {
		t.Fatalf("default project: %v", err)
	}
	left, err := s.ListInProject(ctx, ownerID, def.ID)
	if err != nil {
		t.Fatalf("list apps in default: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("apps still in default = %+v, want none — an app on two "+
			"canvases is two pictures of one system", left)
	}
}

// Re-submitting the form must not scatter the arrangement.
//
// A move to where the app already is writes nothing, because the position is
// cleared as part of moving: without the early return, opening Settings and
// pressing Move would send a card that somebody had placed deliberately back
// into the automatic layout.
func TestMovingAnAppNowhereKeepsItsPosition(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-nomove")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := s.SetPosition(ctx, ownerID, "web", 120, 240); err != nil {
		t.Fatalf("set position: %v", err)
	}

	if err := s.MoveApp(ctx, ownerID, "web", DefaultProjectSlug); err != nil {
		t.Fatalf("MoveApp: %v", err)
	}

	def, err := s.DefaultProject(ctx, ownerID)
	if err != nil {
		t.Fatalf("default project: %v", err)
	}
	apps, err := s.ListInProject(ctx, ownerID, def.ID)
	if err != nil {
		t.Fatalf("list apps in project: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("apps = %+v, want one", apps)
	}
	if apps[0].X == nil || *apps[0].X != 120 ||
		apps[0].Y == nil || *apps[0].Y != 240 {
		t.Errorf("position = (%v, %v), want (120, 240) kept",
			apps[0].X, apps[0].Y)
	}
}

// A project that does not exist is a refusal, not a silent move to the default.
func TestMovingToANonexistentProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-missing")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	err := s.MoveApp(ctx, ownerID, "web", "no-such-project")
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

// An app built from a repository lands on that repository's own canvas, not in
// Default — the reason this feature exists. Before it, a team deploying nine
// apps out of two repositories got one bucket that only said how many there
// were, not which system each came out of.
func TestCreateFromARepositoryMakesAProjectNamedAfterIt(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-project-repo-create")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "api", Source: SourceGit,
		Repo: Repo{URL: "https://github.com/acme/widgets.git"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	p, err := s.ProjectByID(ctx, ownerID, a.ProjectID)
	if err != nil {
		t.Fatalf("project by id: %v", err)
	}
	if p.Slug == DefaultProjectSlug {
		t.Fatalf("app from a repository landed in Default")
	}
	if p.Name != "widgets" || p.Repo != "github.com/acme/widgets" {
		t.Fatalf("project = %+v, want name=widgets repo=github.com/acme/widgets", p)
	}
}

// Six apps out of one repository are one system, not six unrelated ones — the
// project each lands in has to agree regardless of which URL form was pasted
// into which app's [build] block.
func TestAppsFromTheSameRepositoryShareOneProject(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-project-repo-share")

	web, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Source: SourceGit,
		Repo: Repo{URL: "ssh://git@github.com/acme/widgets.git"},
	})
	if err != nil {
		t.Fatalf("create web: %v", err)
	}
	worker, err := s.Create(ctx, ownerID, CreateInput{
		Name: "worker", Source: SourceGit,
		Repo: Repo{URL: "https://github.com/ACME/widgets"},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	if web.ProjectID != worker.ProjectID {
		t.Fatalf("web landed in %s, worker in %s — one repository split into two projects",
			web.ProjectID, worker.ProjectID)
	}

	apps, err := s.ListInProject(ctx, ownerID, web.ProjectID)
	if err != nil {
		t.Fatalf("list apps in project: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("apps in the shared project = %+v, want web and worker", apps)
	}
}

// Two different repositories that happen to end in the same name cannot both
// have the obvious address — the second one falls back to owner/name rather
// than fail the deploy or silently take over the first project.
func TestProjectForRepoFallsBackWhenTheNameIsTaken(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-project-repo-fallback")

	first, err := s.Create(ctx, ownerID, CreateInput{
		Name: "acme-api", Source: SourceGit,
		Repo: Repo{URL: "https://github.com/acme/api.git"},
	})
	if err != nil {
		t.Fatalf("create acme-api: %v", err)
	}
	second, err := s.Create(ctx, ownerID, CreateInput{
		Name: "beta-api", Source: SourceGit,
		Repo: Repo{URL: "https://github.com/beta/api.git"},
	})
	if err != nil {
		t.Fatalf("create beta-api: %v", err)
	}

	if first.ProjectID == second.ProjectID {
		t.Fatal("two different repositories were merged into one project")
	}

	firstProject, err := s.ProjectByID(ctx, ownerID, first.ProjectID)
	if err != nil {
		t.Fatalf("first project: %v", err)
	}
	secondProject, err := s.ProjectByID(ctx, ownerID, second.ProjectID)
	if err != nil {
		t.Fatalf("second project: %v", err)
	}
	if firstProject.Slug != "api" {
		t.Errorf("first project slug = %q, want api", firstProject.Slug)
	}
	if secondProject.Slug != "beta-api" {
		t.Errorf("second project slug = %q, want beta-api (the fallback)", secondProject.Slug)
	}
}

// An app built from an image, not a repository, has nothing to name a project
// after. It is left unassigned exactly as it always was — adopted into
// Default on the next read, the same as any other orphan — rather than routed
// through repository placement it has no repository for.
func TestCreateWithNoRepositoryStillGoesToDefault(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-repo-none")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "cache", Image: "redis:7-alpine", Replicas: 1, Port: 6379,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ProjectID != uuid.Nil {
		t.Fatalf("image-sourced app got project %s at create time, want unassigned", a.ProjectID)
	}

	def, err := s.DefaultProject(ctx, ownerID)
	if err != nil {
		t.Fatalf("default project: %v", err)
	}
	apps, err := s.ListInProject(ctx, ownerID, def.ID)
	if err != nil {
		t.Fatalf("list apps in default: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "cache" {
		t.Fatalf("apps in default = %+v, want the image-sourced app adopted into it", apps)
	}
}

// A caller that named a project explicitly is not second-guessed — a person
// choosing where an app goes on create must win over the automatic placement.
func TestCreateWithAnExplicitProjectIsNotOverridden(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &okBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-project-repo-explicit")

	billing, err := s.CreateProject(ctx, ownerID, "Billing")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "api", Source: SourceGit, ProjectID: billing.ID,
		Repo: Repo{URL: "https://github.com/acme/widgets.git"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ProjectID != billing.ID {
		t.Fatalf("app landed in %s, want the explicitly chosen project %s", a.ProjectID, billing.ID)
	}
}
