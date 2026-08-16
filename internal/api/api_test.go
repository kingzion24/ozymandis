package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/domain"
	"github.com/kingzion24/ozymandis/internal/identity"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// fakeApps is an in-memory app service scoped by owner.
//
// Scoped, rather than a flat map keyed by name, because owner scoping is the
// property most of these tests are about: a fake that ignored ownerID would
// make every tenancy assertion pass regardless of what the handler did.
type fakeApps struct {
	byOwner map[string]map[string]app.App

	created     []app.CreateInput
	deleted     []string
	scaled      map[string]int32
	vars        map[string]string
	deletedVars []string
	redeployed  []string
	health      map[string]string
	commands    map[string]string
	services    map[string]string
	releases    map[string]string

	err error
}

func newFakeApps() *fakeApps {
	return &fakeApps{
		byOwner:  map[string]map[string]app.App{},
		scaled:   map[string]int32{},
		vars:     map[string]string{},
		health:   map[string]string{},
		commands: map[string]string{},
		services: map[string]string{},
		releases: map[string]string{},
	}
}

func (f *fakeApps) add(ownerID string, a app.App) {
	if f.byOwner[ownerID] == nil {
		f.byOwner[ownerID] = map[string]app.App{}
	}
	a.OwnerID = ownerID
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	f.byOwner[ownerID][a.Name] = a
}

func (f *fakeApps) List(_ context.Context, ownerID string) ([]app.App, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []app.App
	for _, a := range f.byOwner[ownerID] {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeApps) Get(_ context.Context, ownerID, name string) (app.App, error) {
	if f.err != nil {
		return app.App{}, f.err
	}
	a, ok := f.byOwner[ownerID][name]
	if !ok {
		return app.App{}, app.ErrNotFound
	}
	return a, nil
}

func (f *fakeApps) Create(_ context.Context, ownerID string, in app.CreateInput) (app.App, error) {
	if f.err != nil {
		return app.App{}, f.err
	}
	f.created = append(f.created, in)
	a := app.App{Name: in.Name, Image: in.Image, Replicas: in.Replicas, Port: in.Port}
	f.add(ownerID, a)
	return f.byOwner[ownerID][in.Name], nil
}

func (f *fakeApps) Scale(_ context.Context, ownerID, name string, replicas int32) (app.App, error) {
	if f.err != nil {
		return app.App{}, f.err
	}
	a, ok := f.byOwner[ownerID][name]
	if !ok {
		return app.App{}, app.ErrNotFound
	}
	f.scaled[name] = replicas
	a.Replicas = replicas
	f.byOwner[ownerID][name] = a
	return a, nil
}

func (f *fakeApps) Redeploy(_ context.Context, ownerID, name string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][name]; !ok {
		return app.ErrNotFound
	}
	f.redeployed = append(f.redeployed, name)
	return nil
}

func (f *fakeApps) Delete(_ context.Context, ownerID, name string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][name]; !ok {
		return app.ErrNotFound
	}
	f.deleted = append(f.deleted, name)
	delete(f.byOwner[ownerID], name)
	return nil
}

func (f *fakeApps) Deployments(
	_ context.Context, _ string, _ uuid.UUID, _ int32,
) ([]app.Deployment, error) {
	return nil, f.err
}

func (f *fakeApps) SetVariable(
	_ context.Context, ownerID, appName string, in app.VariableInput,
) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][appName]; !ok {
		return app.ErrNotFound
	}
	f.vars[in.Key] = in.Value
	return nil
}

func (f *fakeApps) DeleteVariable(_ context.Context, ownerID, appName, key string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][appName]; !ok {
		return app.ErrNotFound
	}
	f.deletedVars = append(f.deletedVars, key)
	return nil
}

func (f *fakeApps) SetHealth(
	_ context.Context, ownerID, name, healthPath string, liveness bool,
) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][name]; !ok {
		return app.ErrNotFound
	}
	f.health[name] = fmt.Sprintf("%s/%v", healthPath, liveness)
	return nil
}

func (f *fakeApps) SetCommand(_ context.Context, ownerID, name, command string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][name]; !ok {
		return app.ErrNotFound
	}
	f.commands[name] = command
	return nil
}

func (f *fakeApps) SetService(
	_ context.Context, ownerID, name string, port int32, internal bool,
) error {
	if f.err != nil {
		return f.err
	}
	a, ok := f.byOwner[ownerID][name]
	if !ok {
		return app.ErrNotFound
	}
	f.services[name] = fmt.Sprintf("%d/%v", port, internal)
	a.Port, a.Internal = port, internal
	f.byOwner[ownerID][name] = a
	return nil
}

func (f *fakeApps) SetReleaseCommand(_ context.Context, ownerID, name, command string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byOwner[ownerID][name]; !ok {
		return app.ErrNotFound
	}
	f.releases[name] = command
	return nil
}

var _ Apps = (*fakeApps)(nil)

// fakeLogs is enough of a log service to exercise the endpoints.
type fakeLogs struct {
	lines []orchestrator.LogLine
	err   error
}

func (f *fakeLogs) Logs(
	_ context.Context, _, _ string, _ app.LogRequest,
) (app.Logs, error) {
	if f.err != nil {
		return app.Logs{}, f.err
	}
	return app.Logs{Pod: "p-1", Pods: []string{"p-1"}, Lines: f.lines}, nil
}

func (f *fakeLogs) LogStream(
	_ context.Context, _, _ string, _ app.LogRequest,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	if f.err != nil {
		return nil, f.err
	}
	return func(yield func(orchestrator.LogLine, error) bool) {
		for _, l := range f.lines {
			if !yield(l, nil) {
				return
			}
		}
	}, nil
}

var _ Logs = (*fakeLogs)(nil)

// tokenIdentity resolves a bearer token to a team, like the real provider.
//
// Resolve and RoleForRequest read separate maps on purpose. In production they
// are two queries against the same rows and are expected to agree, but the
// interesting states are the ones where they briefly do not — a membership
// removed between the two — and a fake that derived one from the other could
// not express that at all.
type tokenIdentity struct {
	tokens map[string]string // token -> team, for Resolve
	roles  map[string]account.Role

	// roleRevoked names tokens the role lookup refuses even though Resolve
	// still recognises them.
	roleRevoked map[string]bool
}

func (t *tokenIdentity) Resolve(_ context.Context, r *http.Request) (identity.Owner, error) {
	team, ok := t.tokens[account.BearerToken(r)]
	if !ok {
		return identity.Owner{}, identity.ErrUnauthenticated
	}
	return identity.Owner{ID: team, DisplayName: team}, nil
}

func (t *tokenIdentity) RoleForRequest(
	_ context.Context, r *http.Request, ownerID string,
) (account.Role, error) {
	raw := account.BearerToken(r)
	if t.roleRevoked[raw] {
		return "", account.ErrTokenNotValid
	}
	team, ok := t.tokens[raw]
	if !ok {
		return "", account.ErrTokenNotValid
	}
	if team != ownerID {
		return "", account.ErrNotAMember
	}
	role, ok := t.roles[raw]
	if !ok {
		role = account.RoleAdmin
	}
	return role, nil
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testServer wires a server with two teams and a token for each.
func testServer(t *testing.T, apps *fakeApps, logs Logs) (http.Handler, *tokenIdentity) {
	t.Helper()
	ident := &tokenIdentity{
		tokens: map[string]string{
			"oz_team-a-token": "team-a",
			"oz_team-b-token": "team-b",
			"oz_member-token": "team-a",
		},
		roles: map[string]account.Role{
			"oz_team-a-token": account.RoleAdmin,
			"oz_team-b-token": account.RoleAdmin,
			"oz_member-token": account.RoleMember,
		},
	}
	srv, err := New(Options{
		Identity: ident, Apps: apps, Roles: ident, Logs: logs, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler(), ident
}

func do(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) Error {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the JSON error envelope (%q): %v", w.Body.String(), err)
	}
	return body.Error
}

// --- Tenancy. The three that matter most. ---

// A token for team A must not be able to READ team B's app — and the refusal
// must be 404, not 403. A 403 confirms the app exists, which turns this
// endpoint into a way to enumerate another team's app names one guess at a
// time.
func TestTokenCannotReadAnotherTeamsApp(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-b", app.App{Name: "secret-service"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodGet, "/api/v1/apps/secret-service", "oz_team-a-token", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (403 would confirm the app exists)", w.Code)
	}
	if got := decodeErr(t, w).Code; got != CodeNotFound {
		t.Errorf("code = %q, want %q", got, CodeNotFound)
	}
	if strings.Contains(w.Body.String(), "secret-service") {
		t.Error("the response echoes the app name back, confirming it exists")
	}
}

// The same for a write. Mutations must not leak existence either.
func TestTokenCannotMutateAnotherTeamsApp(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-b", app.App{Name: "victim"})
	h, _ := testServer(t, apps, nil)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/v1/apps/victim", ""},
		{http.MethodPost, "/api/v1/apps/victim/deploy", "{}"},
		{http.MethodPost, "/api/v1/apps/victim/scale", `{"replicas":9}`},
		{http.MethodPut, "/api/v1/apps/victim/secrets", `{"variables":{"K":"v"}}`},
		{http.MethodDelete, "/api/v1/apps/victim/secrets/K", ""},
	} {
		w := do(h, tc.method, tc.path, "oz_team-a-token", tc.body)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", tc.method, tc.path, w.Code)
		}
	}

	if len(apps.deleted) != 0 || len(apps.redeployed) != 0 || len(apps.scaled) != 0 {
		t.Errorf("another team's app was mutated: deleted=%v redeployed=%v scaled=%v",
			apps.deleted, apps.redeployed, apps.scaled)
	}
	if _, ok := apps.byOwner["team-b"]["victim"]; !ok {
		t.Error("team B's app was deleted by team A")
	}
}

// A credential whose membership has been revoked must resolve to nothing,
// rather than to the team it last acted as.
func TestRevokedCredentialResolvesToNothing(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, ident := testServer(t, apps, nil)

	if w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_team-a-token", ""); w.Code != 200 {
		t.Fatalf("precondition: status = %d, want 200", w.Code)
	}

	// Membership goes. The identity provider stops recognising the token, the
	// way the real one stops when the membership join finds no row.
	delete(ident.tokens, "oz_team-a-token")

	w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_team-a-token", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := decodeErr(t, w).Code; got != CodeUnauthenticated {
		t.Errorf("code = %q, want %q", got, CodeUnauthenticated)
	}
}

// The two lookups must not be able to disagree in the permissive direction.
//
// Authentication and authorisation are separate queries here, and a membership
// removed between them leaves a request that resolves to a team but has no role
// in it. The gate has to take the stricter answer: a credential that
// authenticates but cannot be authorised is refused, not admitted with whatever
// role happens to be the zero value.
func TestARoleThatCannotBeResolvedIsRefused(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, ident := testServer(t, apps, nil)

	if w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_team-a-token", ""); w.Code != 200 {
		t.Fatalf("precondition: status = %d, want 200", w.Code)
	}

	// Resolve still recognises the token; the role lookup no longer does.
	ident.roleRevoked = map[string]bool{"oz_team-a-token": true}

	w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_team-a-token", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — an unresolvable role must refuse, "+
			"not fall through to the zero-value role", w.Code)
	}
	if got := decodeErr(t, w).Code; got != CodeUnauthenticated {
		t.Errorf("code = %q, want %q", got, CodeUnauthenticated)
	}
}

// The empty role must not satisfy any gate. account.Role("").AtLeast(member) is
// false by construction, and this pins that: were rank ever to gain a zero
// entry, every gate in this package would open at once.
func TestTheEmptyRoleSatisfiesNothing(t *testing.T) {
	for _, min := range []account.Role{account.RoleMember, account.RoleAdmin, account.RoleOwner} {
		if account.Role("").AtLeast(min) {
			t.Errorf("the empty role satisfied %q", min)
		}
	}
}

// --- Authentication shape ---

// A CLI must never receive a redirect to an HTML sign-in page. That is the
// single most important difference between this surface and the dashboard.
func TestUnauthenticatedIsJSON401AndNeverARedirect(t *testing.T) {
	h, _ := testServer(t, newFakeApps(), nil)

	for _, path := range []string{
		"/api/v1/whoami", "/api/v1/apps", "/api/v1/apps/web", "/api/v1/apps/web/logs",
	} {
		w := do(h, http.MethodGet, path, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q — a CLI would follow it and parse a login page", path, loc)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: content-type = %q, want JSON", path, ct)
		}
		if got := decodeErr(t, w).Code; got != CodeUnauthenticated {
			t.Errorf("%s: code = %q", path, got)
		}
	}
}

func TestBadTokenIs401(t *testing.T) {
	h, _ := testServer(t, newFakeApps(), nil)
	w := do(h, http.MethodGet, "/api/v1/apps", "oz_nope", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// --- Role gating ---

// A member may read and must not write. The same actions are behind the same
// role on the dashboard; an API that let a member through would be a way around
// the gate rather than a second way to reach it.
func TestMemberMayReadButNotWrite(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	if w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_member-token", ""); w.Code != 200 {
		t.Errorf("member reading: status = %d, want 200", w.Code)
	}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps", `{"name":"new","source":"image","image":"nginx"}`},
		{http.MethodDelete, "/api/v1/apps/web", ""},
		{http.MethodPost, "/api/v1/apps/web/deploy", "{}"},
		{http.MethodPost, "/api/v1/apps/web/scale", `{"replicas":3}`},
		{http.MethodPut, "/api/v1/apps/web/secrets", `{"variables":{"K":"v"}}`},
		{http.MethodDelete, "/api/v1/apps/web/secrets/K", ""},
	} {
		w := do(h, tc.method, tc.path, "oz_member-token", tc.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", tc.method, tc.path, w.Code)
		}
		if got := decodeErr(t, w).Code; got != CodeForbidden {
			t.Errorf("%s %s: code = %q", tc.method, tc.path, got)
		}
	}

	// 403 rather than 404 here is deliberate and the opposite of the tenancy
	// case: the caller is proven to be in this team, so the app's existence is
	// not a secret being kept from them.
	if len(apps.created)+len(apps.deleted)+len(apps.redeployed) != 0 {
		t.Error("a member's write got through")
	}
}

// --- Secrets ---

// A sealed value has no read path anywhere in this codebase, and the API does
// not become the first one.
func TestSecretsNeverReturnAValue(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Variables: []app.Variable{
		{Key: "LOG_LEVEL", Value: "info", Secret: false},
		{Key: "DATABASE_URL", Value: "", Secret: true},
	}})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodGet, "/api/v1/apps/web/secrets", "oz_team-a-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}

	var body struct {
		Variables []Secret `json:"variables"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Variables) != 2 {
		t.Fatalf("got %d variables, want 2", len(body.Variables))
	}
	for _, v := range body.Variables {
		if v.Secret && v.Value != "" {
			t.Errorf("sealed variable %q came back with a value", v.Key)
		}
		if !v.Secret && v.Value == "" {
			t.Errorf("plaintext variable %q lost its value", v.Key)
		}
	}
}

// Setting secrets is additive. A PUT that removed every key it did not mention
// would make `oz secrets set ONE=1` an outage.
func TestSetSecretsIsAdditive(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/secrets", "oz_team-a-token",
		`{"variables":{"A":"1","B":"2"}}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
	}
	if apps.vars["A"] != "1" || apps.vars["B"] != "2" {
		t.Errorf("variables = %v", apps.vars)
	}
	if len(apps.deletedVars) != 0 {
		t.Errorf("a PUT deleted keys it was not asked about: %v", apps.deletedVars)
	}
}

func TestSetSecretsRejectsAnEmptyBody(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	for _, body := range []string{`{}`, `{"variables":{}}`} {
		w := do(h, http.MethodPut, "/api/v1/apps/web/secrets", "oz_team-a-token", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

// --- Request shape ---

// A misspelled field is a caller believing they set something they did not.
func TestUnknownFieldsAreRejected(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPost, "/api/v1/apps/web/scale", "oz_team-a-token",
		`{"replica":3}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a typo'd field must not be silently ignored", w.Code)
	}
}

// {"replicas":0} is a real request — scale to nothing — and must be
// distinguishable from a body that forgot the field.
func TestScaleToZeroIsNotTreatedAsMissing(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Replicas: 3})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPost, "/api/v1/apps/web/scale", "oz_team-a-token", `{"replicas":0}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got, ok := apps.scaled["web"]; !ok || got != 0 {
		t.Errorf("scaled to %v (present=%v), want 0", got, ok)
	}

	w = do(h, http.MethodPost, "/api/v1/apps/web/scale", "oz_team-a-token", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400", w.Code)
	}
}

func TestCreateValidatesBeforeTheService(t *testing.T) {
	apps := newFakeApps()
	h, _ := testServer(t, apps, nil)

	for _, tc := range []struct{ name, body string }{
		{"bad name", `{"name":"Not A Label","source":"image","image":"nginx"}`},
		{"http repo", `{"name":"web","source":"git","repo_url":"http://example.com/x.git"}`},
		{"traversal subdir", `{"name":"web","source":"git","repo_url":"https://e.com/x.git","repo_subdir":"../etc"}`},
		{"negative replicas", `{"name":"web","source":"image","image":"nginx","replicas":-1}`},
		{"unclosed quote", `{"name":"web","source":"image","image":"nginx","command":"sh -c 'oops"}`},
	} {
		w := do(h, http.MethodPost, "/api/v1/apps", "oz_team-a-token", tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, w.Code, w.Body.String())
		}
		if got := decodeErr(t, w).Code; got != CodeInvalid {
			t.Errorf("%s: code = %q, want %q", tc.name, got, CodeInvalid)
		}
	}
	if len(apps.created) != 0 {
		t.Errorf("an invalid create reached the service: %+v", apps.created)
	}
}

// --- Status ---

// "Not running" and "we could not ask" are different facts. Reporting the
// second as the first is the difference between paging somebody and not.
func TestStatusReportsAnUnreachableClusterAsUnavailable(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", StatusKnown: false})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodGet, "/api/v1/apps/web/status", "oz_team-a-token", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := decodeErr(t, w).Code; got != CodeUnavailable {
		t.Errorf("code = %q, want %q", got, CodeUnavailable)
	}
}

func TestStatusReportsWhatTheClusterSaid(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", StatusKnown: true, Status: orchestrator.AppStatus{
		Phase: orchestrator.PhaseRunning, Desired: 3, Ready: 2, Available: 2,
	}})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodGet, "/api/v1/apps/web/status", "oz_team-a-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Phase != "running" || got.Desired != 3 || got.Ready != 2 {
		t.Errorf("status = %+v", got)
	}
}

// --- Router shape ---

// A client that parses every response as JSON should not have to special-case
// the one that says "404 page not found".
func TestUnknownEndpointIsJSON(t *testing.T) {
	h, _ := testServer(t, newFakeApps(), nil)

	w := do(h, http.MethodGet, "/api/v1/nonsense", "oz_team-a-token", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := decodeErr(t, w).Code; got != CodeNotFound {
		t.Errorf("code = %q", got)
	}
}

func TestWrongMethodIsJSON(t *testing.T) {
	h, _ := testServer(t, newFakeApps(), nil)

	w := do(h, http.MethodPatch, "/api/v1/apps", "oz_team-a-token", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if _, err := json.Marshal(decodeErr(t, w)); err != nil {
		t.Errorf("405 body is not the error envelope")
	}
}

// The log endpoints stay off the router entirely when there is no log service,
// rather than being mounted and failing.
func TestLogEndpointsAreAbsentWithoutALogService(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodGet, "/api/v1/apps/web/logs", "oz_team-a-token", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- Logs ---

func TestLogsReturnLines(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	logs := &fakeLogs{lines: []orchestrator.LogLine{
		{At: time.Unix(1, 0).UTC(), Text: "first"},
		{At: time.Unix(2, 0).UTC(), Text: "second"},
	}}
	h, _ := testServer(t, apps, logs)

	w := do(h, http.MethodGet, "/api/v1/apps/web/logs", "oz_team-a-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Pod   string    `json:"pod"`
		Lines []LogLine `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Pod != "p-1" || len(body.Lines) != 2 || body.Lines[0].Text != "first" {
		t.Errorf("body = %+v", body)
	}
}

// Following emits NDJSON — one object per line — not SSE. A CLI reading SSE
// would be stripping "data: " prefixes for no benefit.
func TestFollowingLogsEmitsNDJSON(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	logs := &fakeLogs{lines: []orchestrator.LogLine{
		{At: time.Unix(1, 0).UTC(), Text: "one"},
		{At: time.Unix(2, 0).UTC(), Text: "two"},
	}}
	h, _ := testServer(t, apps, logs)

	w := do(h, http.MethodGet, "/api/v1/apps/web/logs?follow=true", "oz_team-a-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q, want application/x-ndjson", ct)
	}
	if strings.Contains(w.Body.String(), "data:") {
		t.Error("the stream carries SSE framing")
	}

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), w.Body.String())
	}
	for i, raw := range lines {
		var l LogLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Errorf("line %d is not a JSON object: %q", i, raw)
		}
	}
}

// A stream that fails partway has already sent its status, so the failure has
// to arrive in-band. Truncating silently would look like the app exiting.
func TestAStreamErrorIsReportedInBand(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})

	logs := &failingStream{after: orchestrator.LogLine{Text: "before the fault"}}
	h, _ := testServer(t, apps, logs)

	w := do(h, http.MethodGet, "/api/v1/apps/web/logs?follow=true", "oz_team-a-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("the stream ended without saying why: %q", w.Body.String())
	}
}

// failingStream yields one line and then an error.
type failingStream struct{ after orchestrator.LogLine }

func (f *failingStream) Logs(
	_ context.Context, _, _ string, _ app.LogRequest,
) (app.Logs, error) {
	return app.Logs{}, nil
}

func (f *failingStream) LogStream(
	_ context.Context, _, _ string, _ app.LogRequest,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	return func(yield func(orchestrator.LogLine, error) bool) {
		if !yield(f.after, nil) {
			return
		}
		yield(orchestrator.LogLine{}, errors.New("the connection dropped"))
	}, nil
}

func TestLogsRejectAnAbsurdTail(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, &fakeLogs{})

	for _, v := range []string{"0", "-1", "999999", "many"} {
		w := do(h, http.MethodGet, "/api/v1/apps/web/logs?tail="+v, "oz_team-a-token", "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("tail=%s: status = %d, want 400", v, w.Code)
		}
	}
}

// Logs are scoped by owner like everything else.
func TestLogsAreScopedByOwner(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-b", app.App{Name: "victim"})
	h, _ := testServer(t, apps, &fakeLogs{})

	// The fake log service does not scope, so this asserts the handler refuses
	// before reaching it — which is where the real scoping lives too, in the
	// app service's own owner-scoped lookup.
	w := do(h, http.MethodGet, "/api/v1/apps/victim/logs", "oz_team-a-token", "")
	if w.Code == http.StatusOK {
		t.Error("team A read team B's logs")
	}
}

// --- whoami ---

func TestWhoami(t *testing.T) {
	h, _ := testServer(t, newFakeApps(), nil)

	w := do(h, http.MethodGet, "/api/v1/whoami", "oz_member-token", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got Whoami
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TeamID != "team-a" || got.Role != string(account.RoleMember) {
		t.Errorf("whoami = %+v, want team-a/member", got)
	}
}

// --- Options ---

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Options{Apps: newFakeApps()}); err == nil {
		t.Error("a server with no identity provider was accepted")
	}
	if _, err := New(Options{Identity: &tokenIdentity{}}); err == nil {
		t.Error("a server with no apps service was accepted")
	}
}

// --- Error mapping ---

// Every domain error used to fall through to the default case and become a
// 500 saying "check the server log". That is wrong for all of them and
// actively misleading for most: an install with no CNAME target configured,
// or a hostname somebody else already claimed, is not an internal failure and
// the server log has nothing further to say about it. The caller could not
// tell "you cannot do this here" from "this broke".
func TestDomainErrorsAreNotInternalErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
		code string
	}{
		// The install is not configured for this. Not the caller's fault, and
		// not something retrying fixes — the same bucket as ErrNoBuilder.
		{domain.ErrNoTarget, http.StatusServiceUnavailable, CodeUnavailable},
		{domain.ErrNoAppDomain, http.StatusServiceUnavailable, CodeUnavailable},

		// The caller's to fix, and each says how.
		{domain.ErrHostReserved, http.StatusUnprocessableEntity, CodeInvalid},
		{domain.ErrNotVerified, http.StatusUnprocessableEntity, CodeInvalid},

		// Well formed, but somebody else has it.
		{domain.ErrHostTaken, http.StatusConflict, CodeConflict},

		// Nothing there to act on.
		{domain.ErrDomainNotFound, http.StatusNotFound, CodeNotFound},
	} {
		w := httptest.NewRecorder()
		writeServiceError(w, slog.New(slog.DiscardHandler), "add domain", c.err)

		if w.Code != c.want {
			t.Errorf("%v: status = %d, want %d", c.err, w.Code, c.want)
		}

		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: body is not JSON: %v", c.err, err)
		}
		if body.Error.Code != c.code {
			t.Errorf("%v: code = %q, want %q", c.err, body.Error.Code, c.code)
		}

		// The point of the fix: the reason reaches the caller instead of being
		// swapped for a message telling them to read a log they cannot see.
		if body.Error.Message == "something went wrong here; check the server log" {
			t.Errorf("%v: still reported as an internal failure", c.err)
		}
	}
}

// The default case still exists and still hides detail — an error nobody
// mapped is an operator's problem, and its text may name internals.
func TestAnUnmappedErrorIsStillOpaque(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, slog.New(slog.DiscardHandler), "op",
		errors.New("connection to 10.0.0.4:5432 refused"))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "10.0.0.4") {
		t.Error("an internal address reached the caller")
	}
}

// --- Deploy keys ---

// fakePushes mints a predictable key and records who asked.
type fakePushes struct {
	calls int
	err   error
}

func (f *fakePushes) GenerateDeployKey(
	_ context.Context, ownerID, name string,
) (app.DeployKey, error) {
	f.calls++
	if f.err != nil {
		return app.DeployKey{}, f.err
	}
	return app.DeployKey{Public: "ssh-ed25519 AAAAfake ozymandis-" + name}, nil
}

func serverWithPushes(t *testing.T, apps *fakeApps, pushes Pushes) http.Handler {
	t.Helper()
	ident := &tokenIdentity{
		tokens: map[string]string{"oz_team-a-token": "team-a", "oz_member-token": "team-a"},
		roles: map[string]account.Role{
			"oz_team-a-token": account.RoleAdmin,
			"oz_member-token": account.RoleMember,
		},
	}
	srv, err := New(Options{
		Identity: ident, Apps: apps, Roles: ident, Pushes: pushes, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

// The gap this endpoint closes: the API could create an app from a private
// repository but not give it the credential to clone one, so every build failed
// at the clone and the only fix was a dashboard page a script cannot reach.
func TestDeployKeyIsMintedAndOnlyThePublicHalfComesBack(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	pushes := &fakePushes{}
	h := serverWithPushes(t, apps, pushes)

	w := do(h, http.MethodPost, "/api/v1/apps/web/deploy-key", "oz_team-a-token", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body)
	}

	var out DeployKeyOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !strings.HasPrefix(out.Public, "ssh-ed25519 ") {
		t.Errorf("public = %q, want an authorized_keys line", out.Public)
	}

	// The private half has no field to travel in, and must not have acquired
	// one: it is sealed on the way in and unsealed only by a build cloning.
	if strings.Contains(w.Body.String(), "PRIVATE KEY") ||
		strings.Contains(w.Body.String(), "private") {
		t.Error("the response mentions a private key")
	}
}

// Minting a credential is not a read, and a member is not an admin.
func TestDeployKeyNeedsAdmin(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	pushes := &fakePushes{}
	h := serverWithPushes(t, apps, pushes)

	w := do(h, http.MethodPost, "/api/v1/apps/web/deploy-key", "oz_member-token", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if pushes.calls != 0 {
		t.Error("a member's request reached the service")
	}
}

// An install with no secret key has no Pushes wired, and the route is absent
// rather than mounted and failing — the same rule the dashboard's panel follows.
// 404 and not 405: nothing answers that path at all.
func TestDeployKeyIsAbsentWithoutASecretKey(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h := serverWithPushes(t, apps, nil)

	w := do(h, http.MethodPost, "/api/v1/apps/web/deploy-key", "oz_team-a-token", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — the route should not be mounted", w.Code)
	}
}

// Reading the key back must not mint one: regenerating replaces the pair, so a
// read that rotated would revoke the key already working on the repository.
func TestReadingAnAppShowsTheKeyWithoutMintingOne(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", DeployKeyPublic: "ssh-ed25519 AAAAexisting ozymandis-web"})
	pushes := &fakePushes{}
	h := serverWithPushes(t, apps, pushes)

	w := do(h, http.MethodGet, "/api/v1/apps/web", "oz_team-a-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "AAAAexisting") {
		t.Errorf("the stored public key was not returned: %s", w.Body)
	}
	if pushes.calls != 0 {
		t.Error("reading an app minted a deploy key")
	}
}
