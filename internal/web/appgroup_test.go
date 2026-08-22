package web

import (
	"testing"

	"github.com/kingzion24/ozymandis/internal/app"
)

func repoApp(name, url string) app.App {
	return app.App{Name: name, Repo: app.Repo{URL: url}}
}

// The shape of this install: nine apps, six of them out of one repository. A
// flat list cannot say that, which is the whole reason for the grouping.
func TestAppsGroupByTheRepositoryTheyCameFrom(t *testing.T) {
	const chatbot = "ssh://git@github.com/kingzion24/MD_chatbot.git"
	groups := groupAppsByRepo([]app.App{
		repoApp("dashboard", "ssh://git@github.com/harehaDET/mali-daftari-dashboard.git"),
		repoApp("mage", chatbot),
		repoApp("mage-uat", chatbot),
		repoApp("mcp-server", chatbot),
		{Name: "mage-redis", Image: "redis:7-alpine"},
	})

	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(groups), groups)
	}

	// Alphabetical by name, so a heading does not move when an app is added.
	if groups[0].Name != "mali-daftari-dashboard" || groups[1].Name != "MD_chatbot" {
		t.Errorf("groups out of order: %q then %q", groups[0].Name, groups[1].Name)
	}
	if groups[1].Repo != "github.com/kingzion24/MD_chatbot" {
		t.Errorf("group identity = %q", groups[1].Repo)
	}
	if len(groups[1].Apps) != 3 {
		t.Errorf("the chatbot repository has %d apps, want 3", len(groups[1].Apps))
	}

	// The apps no repository claims come last and are not given a name.
	last := groups[len(groups)-1]
	if last.Titled() || len(last.Apps) != 1 || last.Apps[0].Name != "mage-redis" {
		t.Errorf("image-deployed apps are not in the trailing group: %+v", last)
	}
}

// One repository cloned two ways is one heading. Left ungrouped, a team that
// switched from ssh to https halfway through would see their system split.
func TestOneRepositoryIsOneGroupHoweverItWasCloned(t *testing.T) {
	groups := groupAppsByRepo([]app.App{
		repoApp("web", "ssh://git@github.com/KingZion24/MD_chatbot.git"),
		repoApp("worker", "https://github.com/kingzion24/md_chatbot"),
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	if len(groups[0].Apps) != 2 {
		t.Errorf("got %d apps in the group, want 2", len(groups[0].Apps))
	}
	// The first spelling seen is the one shown, rather than a lowercased
	// version of a name whose owner capitalises it.
	if groups[0].Name != "MD_chatbot" {
		t.Errorf("group name = %q, want the spelling first seen", groups[0].Name)
	}
}

// Grouping an install that has never built from source would put the heading
// "Deployed from an image" over every row, which says nothing. That list keeps
// the plain shape it had.
func TestOneHeadingOverEverythingIsNotAGrouping(t *testing.T) {
	images := groupAppsByRepo([]app.App{
		{Name: "redis", Image: "redis:7-alpine"},
		{Name: "cache", Image: "memcached:1"},
	})
	if grouped(images) {
		t.Errorf("apps with no repository were grouped: %+v", images)
	}

	// One repository is still worth naming: the heading says which system the
	// apps under it are, which is more than the list said before.
	one := groupAppsByRepo([]app.App{repoApp("web", "https://github.com/acme/api.git")})
	if !grouped(one) {
		t.Errorf("a single repository was left unnamed: %+v", one)
	}

	if grouped(groupAppsByRepo(nil)) {
		t.Error("an empty list was grouped")
	}
}
