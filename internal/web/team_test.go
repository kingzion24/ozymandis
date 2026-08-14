package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
)

// ------------------------------------------------------------ the switcher
//
// Against the real account service and a real database throughout. Switching
// team is an authorization decision — it changes the owner every later query is
// scoped by — so what has to be proved is that the session moved, or did not,
// and no fake can say that.

// postFormAs sends a form the way the switcher's own markup does.
func (h *liveHarness) postFormAs(
	t *testing.T, path string, c *http.Cookie, form url.Values,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// activeTeam reads the team a cookie's session is acting as, from the database
// rather than from anything the response said about itself.
func (h *liveHarness) activeTeam(t *testing.T, c *http.Cookie) string {
	t.Helper()
	sess, err := h.accounts.ResolveSession(context.Background(), c.Value)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	return sess.ActiveTeamID
}

// team creates a team owned by someone, with an app in it.
func (h *liveHarness) team(t *testing.T, id, name string, owner account.User, appName string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.accounts.CreateTeam(ctx, id, name, owner.ID); err != nil {
		t.Fatalf("create team %s: %v", id, err)
	}
	if appName == "" {
		return
	}
	if _, err := h.apps.Create(ctx, id, app.CreateInput{
		Name: appName, Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("create app %s in %s: %v", appName, id, err)
	}
}

// The one that matters: switching is an authorization decision, not a
// preference. A crafted POST must not move a session into someone else's team.
func TestCannotSwitchIntoATeamYouAreNotIn(t *testing.T) {
	h := newLiveHarness(t, "web-switch-outsider")
	h.installedApp(t, "insider-app")
	h.user(t, "insider@web.test")
	h.user(t, "outsider@web.test")

	// The insider takes the configured team, so the outsider gets one of their
	// own — which is the position anyone signing in to a running install is in.
	if code := h.signIn(t, "insider@web.test").Code; code != http.StatusSeeOther {
		t.Fatalf("the insider's sign-in = %d, want 303", code)
	}
	c := sessionCookie(h.signIn(t, "outsider@web.test"))
	if c == nil {
		t.Fatal("the outsider got no session")
	}
	mine := h.activeTeam(t, c)
	if mine == h.teamID {
		t.Fatalf("the outsider is already acting as %s — there is nothing left to "+
			"switch into", h.teamID)
	}

	rec := h.postFormAs(t, "/teams/switch", c, url.Values{"team": {h.teamID}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /teams/switch into a team they are not in = %d, want 403\n%s",
			rec.Code, rec.Body.String())
	}

	// The status is the smaller half of it. The session must not have moved.
	if now := h.activeTeam(t, c); now != mine {
		t.Fatalf("the session is now acting as %q, want %q — a crafted form moved "+
			"it into a team its holder is not a member of", now, mine)
	}
	// ...and the apps of that team are still not on their screen, which is what
	// the switch would have bought them.
	if body := h.getAs(t, "/apps", c).Body.String(); strings.Contains(body, "insider-app") {
		t.Fatalf("the outsider can see the other team's apps:\n%s", body)
	}
}

// The end-to-end proof that owner_id scoping follows the session: the same
// cookie, the same person, and a different set of apps on the page.
func TestSwitchingChangesWhichAppsAreVisible(t *testing.T) {
	h := newLiveHarness(t, "web-switch-apps")
	h.installedApp(t, "first-app")
	u := h.user(t, "both@web.test")

	c := sessionCookie(h.signIn(t, "both@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	const second = "web-switch-apps-two"
	h.team(t, second, "Second", u, "second-app")

	body := h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "first-app") || strings.Contains(body, "second-app") {
		t.Fatalf("before switching, /apps must show only the first team's apps:\n%s", body)
	}

	rec := h.postFormAs(t, "/teams/switch", c, url.Values{"team": {second}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /teams/switch = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / — the page they were on belongs to the "+
			"team they just left", loc)
	}
	if now := h.activeTeam(t, c); now != second {
		t.Fatalf("active team = %q, want %q", now, second)
	}

	// The same cookie, unchanged: what moved is the session, not the browser.
	body = h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "second-app") {
		t.Errorf("after switching, /apps does not show the new team's apps:\n%s", body)
	}
	if strings.Contains(body, "first-app") {
		t.Errorf("after switching, /apps still shows the old team's apps — owner_id "+
			"scoping did not follow the session:\n%s", body)
	}
}

// A switcher offering a team you are not in is either a broken control or an
// invitation to try the request the test above forbids.
func TestSwitcherListsOnlyYourTeams(t *testing.T) {
	h := newLiveHarness(t, "web-switch-list")
	h.installedApp(t, "list-app")
	u := h.user(t, "lister@web.test")
	stranger := h.user(t, "stranger@web.test")

	c := sessionCookie(h.signIn(t, "lister@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	h.team(t, "web-switch-list-mine", "My Other Team", u, "")
	h.team(t, "web-switch-list-theirs", "Somebody Elses Team", stranger, "")

	body := h.getAs(t, "/apps", c).Body.String()

	// The control is really on the page, or the absences below prove nothing.
	if !strings.Contains(body, `action="/teams/switch"`) {
		t.Fatalf("no team switcher on the page:\n%s", body)
	}
	for _, want := range []string{
		`value="web-switch-list"`, `value="web-switch-list-mine"`, "My Other Team",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the switcher is missing %q — a team the person belongs to is "+
				"not offered", want)
		}
	}
	for _, unwanted := range []string{"web-switch-list-theirs", "Somebody Elses Team"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the switcher offers %q, which belongs to somebody else:\n%s",
				unwanted, body)
		}
	}
}

// GET must not switch anyone's team: a prefetched or crawled link would, and
// landing in another team without asking is both confusing and a way to make
// somebody act in a team they did not choose.
func TestSwitchRejectsGET(t *testing.T) {
	h := newLiveHarness(t, "web-switch-get")
	h.installedApp(t, "get-app")
	u := h.user(t, "getter@web.test")

	c := sessionCookie(h.signIn(t, "getter@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	const second = "web-switch-get-two"
	h.team(t, second, "Second", u, "")
	before := h.activeTeam(t, c)

	rec := h.getAs(t, "/teams/switch?team="+second, c)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /teams/switch = %d, want 405", rec.Code)
	}
	if now := h.activeTeam(t, c); now != before {
		t.Fatalf("a GET moved the session from %q to %q", before, now)
	}
}

// ------------------------------------------------------ team management
//
// Against the real account service and a real database throughout. Every claim
// here — that a session dies with its membership, that a revoked invitation
// stops working, that the last owner cannot be removed — is a fact about rows
// in three tables, and a fake asked the same questions would only repeat the
// answers it was written with.

// invite sends the management page's own invitation form.
func (h *liveHarness) invite(
	t *testing.T, c *http.Cookie, email, role string,
) *httptest.ResponseRecorder {
	t.Helper()
	return h.postFormAs(t, "/team/invite", c, url.Values{
		"email": {email}, "role": {role},
	})
}

// invitationToken pulls the token out of the invitation mail, which is the only
// place it exists — the same position the invited person is in.
func invitationToken(t *testing.T, body string) string {
	t.Helper()
	const prefix = "https://ozymandis.test/invitations/"
	i := strings.Index(body, prefix)
	if i < 0 {
		t.Fatalf("no invitation link in the mail:\n%s", body)
	}
	token := body[i+len(prefix):]
	if j := strings.IndexAny(token, " \r\n"); j >= 0 {
		token = token[:j]
	}
	if token == "" {
		t.Fatalf("the invitation link carries no token:\n%s", body)
	}
	return token
}

// lastMail is the message the action under test caused, or a failure saying it
// caused none.
func (h *liveHarness) lastMail(t *testing.T, before int) string {
	t.Helper()
	sent := h.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("mails sent = %d, want one more than %d", len(sent), before)
	}
	return sent[len(sent)-1].TextBody
}

// invitationAction finds the id in a revoke form, which is how the page offers
// an invitation to be withdrawn.
var invitationAction = regexp.MustCompile(
	`/team/invitations/([0-9a-fA-F-]{36})/revoke`)

func TestRemovingAMemberEndsTheirSession(t *testing.T) {
	rt := newRoleTeam(t, "web-team-remove", "remove-app")

	if code := rt.getAs(t, "/apps", rt.member).Code; code != http.StatusOK {
		t.Fatalf("GET /apps as the member before removal = %d, want 200", code)
	}

	rec := rt.postFormAs(t, "/team/members/"+rt.memberID.String()+"/remove", rt.owner, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST remove = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	if rec := rt.getAs(t, "/apps", rt.member); !deniedGET(t, rec, "web-") {
		t.Fatalf("GET /apps as the removed member = %d — their cookie still "+
			"opens the team they were taken out of", rec.Code)
	}
	if body := rt.getAs(t, "/team", rt.owner).Body.String(); strings.Contains(
		body, "member@web.test") {
		t.Errorf("the removed member is still listed:\n%s", body)
	}
	if _, err := rt.accounts.RoleIn(
		context.Background(), rt.memberID, rt.teamID,
	); !errors.Is(err, account.ErrNotAMember) {
		t.Fatalf("role after removal: %v, want ErrNotAMember", err)
	}
}

// A team with no owner cannot be administered by anyone, ever again — there is
// no route back that does not go through the database.
func TestCannotRemoveOrDemoteTheLastOwner(t *testing.T) {
	rt := newRoleTeam(t, "web-team-lastowner", "last-app")
	ctx := context.Background()

	stillOwner := func(what string) {
		t.Helper()
		role, err := rt.accounts.RoleIn(ctx, rt.ownerID, rt.teamID)
		if err != nil || role != account.RoleOwner {
			t.Fatalf("after %s the only owner holds %q (%v), want owner — the team "+
				"can no longer be administered by anybody", what, role, err)
		}
	}

	demote := "/team/members/" + rt.ownerID.String() + "/role"
	if rec := rt.postFormAs(t, demote, rt.owner, url.Values{
		"role": {"member"},
	}); rec.Code < 400 {
		t.Errorf("the last owner demoted themselves: %d\n%s", rec.Code, rec.Body.String())
	}
	stillOwner("a self-demotion")

	remove := "/team/members/" + rt.ownerID.String() + "/remove"
	if rec := rt.postFormAs(t, remove, rt.owner, nil); rec.Code < 400 {
		t.Errorf("the last owner removed themselves: %d\n%s", rec.Code, rec.Body.String())
	}
	stillOwner("a self-removal")
	if code := rt.getAs(t, "/apps", rt.owner).Code; code != http.StatusOK {
		t.Errorf("GET /apps as the owner = %d, want 200 — the refused removal took "+
			"their session with it", code)
	}

	// The refusal is about the team keeping an owner, not about owners being
	// unchangeable: with a second one appointed, the same demotion goes through.
	if rec := rt.postFormAs(t, "/team/members/"+rt.adminID.String()+"/role", rt.owner,
		url.Values{"role": {"owner"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("appointing a second owner = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if rec := rt.postFormAs(t, demote, rt.owner, url.Values{
		"role": {"member"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("demoting one of two owners = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if role, err := rt.accounts.RoleIn(ctx, rt.ownerID, rt.teamID); err != nil ||
		role != account.RoleMember {
		t.Fatalf("role after the demotion = %q (%v), want member", role, err)
	}
}
