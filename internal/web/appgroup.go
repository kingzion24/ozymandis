package web

import (
	"sort"
	"strings"

	"github.com/kingzion24/ozymandis/internal/app"
)

// AppGroup is one repository's apps, as the list draws them.
//
// The grouping is derived on render rather than stored. Every app that builds
// from source already carries the URL it builds from, so which repository
// produced a workload is a fact the database has had all along — it was simply
// never shown. Storing a second copy of it would be a second thing that can
// disagree with the first.
type AppGroup struct {
	// Name is the repository's own name, as its owner spells it —
	// "mali-daftari-dashboard", "MD_chatbot". Empty for the apps that were not
	// built from a repository at all.
	Name string

	// Repo is the whole identity, "github.com/owner/name", shown beside the
	// name. Two forges can carry the same owner and name, and a heading
	// claiming "acme/api" over apps built from a private GitLab would be a
	// label for the wrong system.
	Repo string

	Apps []app.App
}

// Titled reports whether this group stands for a repository, as opposed to
// holding the apps that came from none.
func (g AppGroup) Titled() bool { return g.Name != "" }

// groupAppsByRepo splits a team's apps by the repository each was built from.
//
// A flat list answers "what is running" and nothing else. Past a handful of
// workloads the question people actually arrive with is "which of these belong
// to the chatbot" — six of nine apps here come out of one repository — and the
// only way to read that off a flat list is to already know the answer.
//
// Grouped by repository rather than by branch or environment: an app built
// from main and its staging twin built from a release branch are two
// deployments of one system, and separating them would put the two halves of a
// promotion under different headings.
func groupAppsByRepo(apps []app.App) []AppGroup {
	// Keyed on the lowercased identity so one repository cloned over ssh by one
	// app and https by the next is one group. The first spelling seen wins for
	// display, which is arbitrary only between spellings that mean the same
	// thing.
	index := map[string]int{}
	var groups []AppGroup
	var loose []app.App

	for _, a := range apps {
		id := a.Repo.Identity()
		if !id.Set() {
			loose = append(loose, a)
			continue
		}
		key := id.Key()
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, AppGroup{Name: id.Name, Repo: id.String()})
		}
		groups[i].Apps = append(groups[i].Apps, a)
	}

	// Alphabetical, case-insensitively: the list is scanned for a name somebody
	// already has in mind, and ordering by size or by age would move a heading
	// every time an app was added.
	sort.SliceStable(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})

	// Last, and only when there is something to put in it. These are the
	// databases and caches deployed from an image — real apps, but not ones any
	// repository claims, and heading the list with them would bury the systems.
	if len(loose) > 0 {
		groups = append(groups, AppGroup{Apps: loose})
	}
	return groups
}

// grouped reports whether splitting these apps by repository would say
// anything.
//
// One heading over the whole list is not a grouping, it is a caption — and on
// an install that only ever deploys prebuilt images it would be the caption
// "No repository" over everything. That list is better off as it was.
func grouped(groups []AppGroup) bool {
	return len(groups) > 1 || (len(groups) == 1 && groups[0].Titled())
}
