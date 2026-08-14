package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/identity"
)

// chromeFor builds the chrome for a request carrying any of owner, viewer and
// teams, so a test can say which of them exist and assert on what is drawn.
func chromeFor(owner *identity.Owner, viewer *Viewer, teams []TeamChoice) Slots {
	ctx := context.Background()
	if owner != nil {
		ctx = identity.NewContext(ctx, *owner)
	}
	if viewer != nil {
		v := *viewer
		ctx = context.WithValue(ctx, viewerKey{}, viewerLookup(func() (Viewer, bool) {
			return v, true
		}))
	}
	if teams != nil {
		ctx = context.WithValue(ctx, teamsKey{}, teamLister(func() []TeamChoice {
			return teams
		}))
	}
	return DefaultSlots{}.Slots(ctx, httptest.NewRequest("GET", "/", nil))
}

// The footer names the person, not the install.
//
// Both are on the context and they answer different questions: the owner is the
// team every query is scoped by, the viewer is who is looking. Before sign-in
// existed there was only the first, so the footer showed it — and after signing
// in as somebody the sidebar still read "Local", which names the install rather
// than the person in front of it.
func TestSidebarFooterNamesThePersonNotTheTeam(t *testing.T) {
	owner := identity.Owner{ID: "owner-local", DisplayName: "Local"}
	viewer := Viewer{UserID: uuid.New(), Username: "batman", IsSuperuser: true}

	got := render(t, chromeFor(&owner, &viewer, []TeamChoice{{ID: "owner-local", Name: "Local"}}))

	if !strings.Contains(got, "batman") {
		t.Fatalf("the footer does not name the signed-in person:\n%s", got)
	}
	if strings.Contains(got, "Local") {
		t.Fatalf("the footer still names the team — the switcher above already does:\n%s", got)
	}
}

// A superuser is told so where they can see it.
//
// It decides whether the people-management block appears on the team page, and
// somebody who cannot see the block has no other way to find out why.
func TestSidebarFooterSaysWhenTheViewerIsASuperuser(t *testing.T) {
	viewer := Viewer{UserID: uuid.New(), Username: "batman", IsSuperuser: true}
	if got := render(t, chromeFor(nil, &viewer, nil)); !strings.Contains(got, "superuser") {
		t.Fatalf("the footer does not mark a superuser:\n%s", got)
	}

	ordinary := Viewer{UserID: uuid.New(), Username: "robin"}
	if got := render(t, chromeFor(nil, &ordinary, nil)); strings.Contains(got, "superuser") {
		t.Fatalf("an ordinary user is marked a superuser:\n%s", got)
	}
}

// A display name is what to call somebody; the username is what they sign in
// with. Showing the first without the second leaves an administrator unable to
// tell which account they are looking at.
func TestSidebarFooterShowsBothNamesWhenTheyDiffer(t *testing.T) {
	viewer := Viewer{UserID: uuid.New(), Username: "robin", DisplayName: "Dick Grayson"}

	got := render(t, chromeFor(nil, &viewer, nil))
	for _, want := range []string{"Dick Grayson", "robin"} {
		if !strings.Contains(got, want) {
			t.Errorf("the footer does not carry %q:\n%s", want, got)
		}
	}
}

// The fallback is not dead code.
//
// An install authenticating by bearer token, or one with no token at all, has
// an owner and no person. The footer there has to show the owner, because it is
// the only true thing available — and a sidebar with an empty corner reads as a
// broken control.
func TestSidebarFooterFallsBackToTheOwnerWithNoViewer(t *testing.T) {
	owner := identity.Owner{ID: "owner-local", DisplayName: "Local"}

	got := render(t, chromeFor(&owner, nil, nil))
	if !strings.Contains(got, "Local") {
		t.Fatalf("a token install draws no name at all:\n%s", got)
	}
}

// Every page used to have the same document title, so a row of tabs was a row
// of tabs nobody could tell apart — and browser history recorded the same entry
// for every page anybody had ever visited.
func TestPageTitleNamesThePage(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/", "Overview · Ozymandis"},
		{"/deployments", "Deployments · Ozymandis"},
		{"/settings", "Settings · Ozymandis"},
		{"/settings/backups", "Backups · Ozymandis"},
		{"/cluster/volumes", "Volumes · Ozymandis"},
	} {
		if got := pageTitle(tc.path); got != tc.want {
			t.Errorf("pageTitle(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// Every page the navigation offers must have a title.
//
// The title is derived from the breadcrumb, so a page with no breadcrumb case
// gets the bare product name and is indistinguishable from every other untitled
// page — which is exactly what /team did until this test existed. Walking the
// navigation rather than a hand-written list is what keeps a page added later
// from being missed.
func TestEveryNavTargetHasATitle(t *testing.T) {
	slots := chromeFor(&identity.Owner{ID: "owner-local", DisplayName: "Local"}, nil, nil)

	seen := map[string]bool{}
	for _, group := range slots.Nav {
		for _, item := range group.Items {
			if item.Href == "" || seen[item.Href] {
				continue
			}
			seen[item.Href] = true
			if got := pageTitle(item.Href); got == DefaultBrandName {
				t.Errorf("%s has no title of its own — add a breadcrumb case for it", item.Href)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no navigation targets were checked")
	}
}

// The username stays out of the tab.
//
// A document title is read by browser history, by session restore, and by
// anything that lists open windows during a screen share. None of those are
// places an account name has to appear, and the sidebar shows it to anybody
// actually looking at the page.
func TestPageTitleCarriesNoUsername(t *testing.T) {
	viewer := Viewer{UserID: uuid.New(), Username: "batman", IsSuperuser: true}

	slots := chromeFor(&identity.Owner{ID: "owner-local", DisplayName: "Local"}, &viewer, nil)
	if strings.Contains(slots.Title, "batman") {
		t.Fatalf("the document title carries the username: %q", slots.Title)
	}
	if slots.Title != "Overview · Ozymandis" {
		t.Fatalf("Title = %q, want the page and the product", slots.Title)
	}
}
