package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/appspec"
)

func mustSpec(t *testing.T, doc string) appspec.Spec {
	t.Helper()
	s, err := appspec.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse(%q): %v", doc, err)
	}
	return s
}

// find returns the change for a field, and whether there was one.
func find(res ConfigResult, field string) (Change, bool) {
	for _, c := range res.Changes {
		if c.Field == field {
			return c, true
		}
	}
	return Change{}, false
}

// --- The two axes ---

// The operational axis. A deploy from a stale checkout must not undo an
// emergency scale, and must say so rather than silently declining.
func TestReplicasIsOperationalAndNotConvergedByDefault(t *testing.T) {
	a := app.App{Name: "web", Replicas: 10} // scaled up during an incident
	spec := mustSpec(t, "name = \"web\"\n[scale]\nreplicas = 2\n")

	res := plan(a, spec, nil, false, true)

	c, ok := find(res, "scale.replicas")
	if !ok {
		t.Fatal("the difference was not reported at all — a converge that " +
			"silently declines is indistinguishable from one that acted")
	}
	if !c.Skipped {
		t.Error("replicas was converged without --scale; an emergency scale-up " +
			"would have been reverted by an ordinary deploy")
	}
	if c.Axis != axisOperational {
		t.Errorf("axis = %q, want %q", c.Axis, axisOperational)
	}
	if !strings.Contains(c.Reason, "--scale") {
		t.Errorf("the reason does not say how to apply it: %q", c.Reason)
	}
	if c.From != "10" || c.To != "2" {
		t.Errorf("change = %+v, want from 10 to 2", c)
	}
}

// And with --scale it applies.
func TestReplicasIsConvergedWithScale(t *testing.T) {
	a := app.App{Name: "web", Replicas: 10}
	spec := mustSpec(t, "name = \"web\"\n[scale]\nreplicas = 2\n")

	res := plan(a, spec, nil, true, true)

	c, ok := find(res, "scale.replicas")
	if !ok {
		t.Fatal("no change reported")
	}
	if c.Skipped {
		t.Error("--scale did not apply the file's value")
	}
}

// The case the pointer discipline exists for, at the convergence layer rather
// than the decoder: an explicit `replicas = 0` is a real instruction, and a
// missing [scale] is not. If these ever collapse, a deploy of a file that says
// nothing about scaling takes production to zero.
func TestExplicitZeroReplicasIsNotTheSameAsAbsent(t *testing.T) {
	a := app.App{Name: "web", Replicas: 3}

	explicit := plan(a, mustSpec(t, "name = \"web\"\n[scale]\nreplicas = 0\n"), nil, true, true)
	c, ok := find(explicit, "scale.replicas")
	if !ok {
		t.Fatal("an explicit `replicas = 0` produced no change — " +
			"scale-to-nothing was read as say-nothing")
	}
	if c.To != "0" {
		t.Errorf("To = %q, want 0", c.To)
	}

	// No [scale] table at all: nothing to do, even with --scale.
	absent := plan(a, mustSpec(t, "name = \"web\"\n"), nil, true, true)
	if _, ok := find(absent, "scale.replicas"); ok {
		t.Error("a file that says nothing about scaling produced a scale change — " +
			"this is the deploy that takes production to zero")
	}

	// [scale] present but empty: also nothing to do.
	empty := plan(a, mustSpec(t, "name = \"web\"\n[scale]\n"), nil, true, true)
	if _, ok := find(empty, "scale.replicas"); ok {
		t.Error("an empty [scale] table produced a scale change")
	}
}

// The declarative axis: drift in the dashboard is reverted.
func TestDeclarativeFieldsAreConverged(t *testing.T) {
	a := app.App{
		Name: "web", Port: 3000, Internal: true,
		HealthPath: "/old", Liveness: false,
	}
	spec := mustSpec(t, `
name = "web"
[service]
port     = 8080
internal = false
[health]
path     = "/healthz"
liveness = true
`)

	res := plan(a, spec, nil, false, true)

	for _, want := range []struct{ field, from, to string }{
		{"service.port", "3000", "8080"},
		{"service.internal", "true", "false"},
		{"health.path", "/old", "/healthz"},
		{"health.liveness", "false", "true"},
	} {
		c, ok := find(res, want.field)
		if !ok {
			t.Errorf("%s: no change reported", want.field)
			continue
		}
		if c.Skipped {
			t.Errorf("%s: skipped, but the declarative axis is converged", want.field)
		}
		if c.Axis != axisDeclarative {
			t.Errorf("%s: axis = %q", want.field, c.Axis)
		}
		if c.From != want.from || c.To != want.to {
			t.Errorf("%s: %q -> %q, want %q -> %q", want.field, c.From, c.To, want.from, want.to)
		}
	}
}

// [build] changes are reported as skipped, never as changes this will make.
//
// The preview promising something apply cannot do is the exact failure this
// endpoint exists to prevent, and it is worse than not reporting at all: a
// person edits the image, converges, sees the diff say it changed, and believes
// it.
//
// The reason must name the verb that DOES apply it. "Not supported" reads as a
// permanent wall and sends somebody looking for a workaround, when what they
// need is `oz deploy`.
func TestBuildChangesAreDeferredToADeploy(t *testing.T) {
	a := app.App{Name: "web", Image: "nginx:1"}
	spec := mustSpec(t, "name = \"web\"\n[build]\nimage = \"nginx:2\"\n")

	res := plan(a, spec, nil, false, true)

	c, ok := find(res, "build.image")
	if !ok {
		t.Fatal("the difference was not reported at all — a file that disagrees " +
			"with its app would look like one that matches")
	}
	if !c.Skipped {
		t.Error("reported as a change the converge will make, but a build change " +
			"belongs to the deploy path")
	}
	if !strings.Contains(c.Reason, "deploy") {
		t.Errorf("the reason does not name the verb that applies it: %q", c.Reason)
	}
	if strings.Contains(c.Reason, "not supported") {
		t.Errorf("the reason reads as a permanent wall rather than a different "+
			"verb: %q", c.Reason)
	}
}

// Every [build] field, not just image.
func TestEveryBuildFieldIsDeferred(t *testing.T) {
	a := app.App{Name: "web", Repo: app.Repo{
		URL: "https://e.com/a.git", Branch: "main", Subdir: "svc",
	}}
	spec := mustSpec(t, `
name = "web"
[build]
repo   = "https://e.com/b.git"
branch = "next"
subdir = "other"
`)

	res := plan(a, spec, nil, false, true)

	for _, field := range []string{"build.repo", "build.branch", "build.subdir"} {
		c, ok := find(res, field)
		if !ok {
			t.Errorf("%s: not reported", field)
			continue
		}
		if !c.Skipped || !strings.Contains(c.Reason, "deploy") {
			t.Errorf("%s: %+v", field, c)
		}
	}
}

// The invariant behind both of the above, checked structurally rather than
// case by case: every change the plan does NOT mark skipped must have a branch
// in apply. A field reported as convergeable with no code to converge it is a
// preview that lies, and adding one is a mistake nobody would see in review.
func TestEveryUnskippedChangeHasAnApplyBranch(t *testing.T) {
	// An app and a spec that differ in every field the planner knows about.
	a := app.App{
		Name: "web", Port: 3000, Internal: true, Image: "nginx:1", Replicas: 1,
		HealthPath: "/old", Liveness: false,
		Variables: []app.Variable{{Key: "DROP", Value: "x"}},
	}
	spec := mustSpec(t, `
name = "web"
[service]
port     = 8080
internal = false
[build]
image = "nginx:2"
[health]
path     = "/healthz"
liveness = true
[scale]
replicas = 5
[env]
ADD = "1"
[[domains]]
host = "new.example.com"
`)

	res := plan(a, spec, []string{"old.example.com"}, true, true)
	if len(res.Changes) == 0 {
		t.Fatal("the fixture produced no changes")
	}

	// Every field apply has a branch for. Kept in step with the switch in
	// apply by hand — which is what this test makes visible.
	appliable := map[string]bool{
		"deploy.release_command": true,
		"service.port":           true,
		"service.internal":       true,
		"health.path":            true,
		"health.liveness":        true,
		"scale.replicas":         true,
		"domains":                true,
		"env.*":                  true,
	}

	for _, c := range res.Changes {
		if c.Skipped {
			if c.Reason == "" {
				t.Errorf("%s is skipped with no reason given", c.Field)
			}
			continue
		}
		field := c.Field
		if strings.HasPrefix(field, "env.") {
			field = "env.*"
		}
		if !appliable[field] {
			t.Errorf("%s is reported as a change that will be applied, but apply "+
				"has no branch for it. Either add one, or mark it skipped with a "+
				"reason — a preview that promises what apply will not do is worse "+
				"than one that reports nothing.", c.Field)
		}
	}
}

// A field the file does not mention is left alone, on either axis.
func TestUnmentionedFieldsAreLeftAlone(t *testing.T) {
	a := app.App{
		Name: "web", Port: 3000, Internal: true, Replicas: 4,
		HealthPath: "/healthz", Liveness: true,
	}
	res := plan(a, mustSpec(t, "name = \"web\"\n"), nil, true, true)

	if len(res.Changes) != 0 {
		t.Errorf("a file naming nothing produced %d changes: %+v",
			len(res.Changes), res.Changes)
	}
}

// --- env ---

// Plaintext env is fully declarative: the file can describe the complete set,
// so keys it does not name are removed.
func TestEnvIsFullyDeclarative(t *testing.T) {
	a := app.App{Name: "web", Variables: []app.Variable{
		{Key: "KEEP", Value: "old", Secret: false},
		{Key: "DROP", Value: "gone", Secret: false},
	}}
	spec := mustSpec(t, "name = \"web\"\n[env]\nKEEP = \"new\"\nADD = \"1\"\n")

	res := plan(a, spec, nil, false, true)

	if c, ok := find(res, "env.KEEP"); !ok || c.To != "new" {
		t.Errorf("KEEP: %+v ok=%v", c, ok)
	}
	if c, ok := find(res, "env.ADD"); !ok || c.To != "1" {
		t.Errorf("ADD: %+v ok=%v", c, ok)
	}
	c, ok := find(res, "env.DROP")
	if !ok {
		t.Fatal("a key absent from the file was not removed — " +
			"the file owns plaintext env completely")
	}
	if c.To != "" || c.From != "gone" {
		t.Errorf("DROP: %+v", c)
	}
}

// A sealed variable is not in the spec and must never be touched by a
// converge. The file cannot describe secrets, so it does not own them.
func TestConvergingNeverTouchesSecrets(t *testing.T) {
	a := app.App{Name: "web", Variables: []app.Variable{
		{Key: "DATABASE_URL", Value: "", Secret: true},
		{Key: "LOG_LEVEL", Value: "info", Secret: false},
	}}
	// An [env] table naming neither: LOG_LEVEL goes, the secret stays.
	spec := mustSpec(t, "name = \"web\"\n[env]\n")

	res := plan(a, spec, nil, false, true)

	if _, ok := find(res, "env.DATABASE_URL"); ok {
		t.Error("a converge proposed to change a sealed variable")
	}
	if _, ok := find(res, "env.LOG_LEVEL"); !ok {
		t.Error("an empty [env] table did not remove the plaintext variable")
	}
}

// No [env] table at all means "say nothing", not "remove everything".
func TestAbsentEnvRemovesNothing(t *testing.T) {
	a := app.App{Name: "web", Variables: []app.Variable{
		{Key: "A", Value: "1", Secret: false},
		{Key: "B", Value: "2", Secret: false},
	}}
	res := plan(a, mustSpec(t, "name = \"web\"\n"), nil, false, true)

	for _, c := range res.Changes {
		if strings.HasPrefix(c.Field, "env.") {
			t.Errorf("a file with no [env] table proposed %+v", c)
		}
	}
}

// KEY = "" is a variable set to empty; KEY absent is a variable removed. Both
// render as To == "", so anything branching on the rendered value rather than
// on presence would delete a variable somebody deliberately blanked.
func TestEmptyValueIsNotRemoval(t *testing.T) {
	a := app.App{Name: "web", Variables: []app.Variable{
		{Key: "FLAG", Value: "on", Secret: false},
	}}

	blanked := mustSpec(t, "name = \"web\"\n[env]\nFLAG = \"\"\n")
	if _, ok := blanked.Env["FLAG"]; !ok {
		t.Fatal("the spec lost the blanked key entirely")
	}

	removed := mustSpec(t, "name = \"web\"\n[env]\n")
	if _, ok := removed.Env["FLAG"]; ok {
		t.Fatal("the spec invented a key")
	}

	// Both produce a change rendering to "", which is exactly why apply must
	// consult the map rather than the diff.
	for _, s := range []appspec.Spec{blanked, removed} {
		c, ok := find(plan(a, s, nil, false, true), "env.FLAG")
		if !ok || c.To != "" {
			t.Fatalf("expected a change to \"\": %+v ok=%v", c, ok)
		}
	}
}

// The half of the above that actually matters, and which the plan-only version
// of this test did not reach.
//
// plan renders both cases identically, so the distinction survives only if
// apply consults the map. Exercised through the endpoint rather than by calling
// apply directly, because the bug being guarded against lives in the step
// between the two — and a test that stops at plan passes while apply deletes a
// variable somebody deliberately blanked.
func TestApplyDistinguishesBlankedFromRemoved(t *testing.T) {
	t.Run("blanked is set, not deleted", func(t *testing.T) {
		apps := newFakeApps()
		apps.add("team-a", app.App{Name: "web", Variables: []app.Variable{
			{Key: "FLAG", Value: "on", Secret: false},
		}})
		h, _ := testServer(t, apps, nil)

		w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
			`{"spec":{"name":"web","env":{"FLAG":""}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}

		if len(apps.deletedVars) != 0 {
			t.Errorf("a deliberately blanked variable was DELETED: %v", apps.deletedVars)
		}
		got, ok := apps.vars["FLAG"]
		if !ok {
			t.Fatal("FLAG was not set at all")
		}
		if got != "" {
			t.Errorf("FLAG = %q, want the empty string", got)
		}
	})

	t.Run("absent is deleted, not blanked", func(t *testing.T) {
		apps := newFakeApps()
		apps.add("team-a", app.App{Name: "web", Variables: []app.Variable{
			{Key: "FLAG", Value: "on", Secret: false},
		}})
		h, _ := testServer(t, apps, nil)

		w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
			`{"spec":{"name":"web","env":{}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}

		if _, ok := apps.vars["FLAG"]; ok {
			t.Errorf("a removed variable was SET to empty instead of deleted: %v", apps.vars)
		}
		if len(apps.deletedVars) != 1 || apps.deletedVars[0] != "FLAG" {
			t.Errorf("deleted = %v, want [FLAG]", apps.deletedVars)
		}
	})
}

// service.port and service.internal must reach the SERVICE, not just the plan.
//
// The plan-only version of this assertion was green for the whole of the first
// pass at 1c, while apply had no branch for either field and the port silently
// never changed. A plan-only assertion cannot tell a converged field from a
// reported one, so this goes through the endpoint and checks the service method
// was called — the same shape as TestApplyDistinguishesBlankedFromRemoved.
func TestServiceFieldsReachApply(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Port: 3000, Internal: true})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","service":{"port":8080,"internal":false}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	got, ok := apps.services["web"]
	if !ok {
		t.Fatal("SetService was never called — the change was reported and not made")
	}
	if got != "8080/false" {
		t.Errorf("SetService called with %q, want \"8080/false\"", got)
	}

	// And the app really carries the new values afterwards.
	after := apps.byOwner["team-a"]["web"]
	if after.Port != 8080 || after.Internal {
		t.Errorf("app = port %d internal %v, want 8080/false", after.Port, after.Internal)
	}
}

// Only one of the pair named: the other must keep its current value rather
// than being reset to a zero.
func TestSettingOnlyThePortKeepsInternal(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Port: 3000, Internal: true})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","service":{"port":8080}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := apps.services["web"]; got != "8080/true" {
		t.Errorf("SetService called with %q, want \"8080/true\" — a field the "+
			"file did not name was reset", got)
	}
}

// An empty [service] table must be a no-op, not a write of two zeros.
//
// The mechanism should give this for free — both Port and Internal are nil, so
// neither `if` in plan fires — but this is the exact absent-versus-zero case the
// whole stage exists to get right, and "it follows from the pointers" is the
// reasoning that stops being true the day somebody adds a convenience default.
// Port 0 with internal false would be an app taking no traffic on no port.
func TestAnEmptyServiceTableIsANoOp(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Port: 8080, Internal: true})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","service":{}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	if len(apps.services) != 0 {
		t.Errorf("an empty [service] table called SetService with %v — "+
			"absent collapsed into zero", apps.services)
	}
	after := apps.byOwner["team-a"]["web"]
	if after.Port != 8080 || !after.Internal {
		t.Errorf("app = port %d internal %v, want 8080/true unchanged",
			after.Port, after.Internal)
	}

	// And the plan says nothing about either field, so no diff line either.
	var res ConfigResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range res.Changes {
		if strings.HasPrefix(c.Field, "service.") {
			t.Errorf("an empty [service] table produced %+v", c)
		}
	}
}

// The release command converges, and reaches the service rather than only the
// plan.
//
// [deploy] is declarative despite naming a command: it changes what a deploy
// DOES, not what the app IS, so it belongs on this axis rather than deferred
// with [build].
func TestReleaseCommandReachesApply(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","deploy":{"release_command":"./bin/migrate"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := apps.releases["web"]; got != "./bin/migrate" {
		t.Errorf("SetReleaseCommand got %q — the change was reported and not made", got)
	}
}

// An empty release_command clears it, and is not the same as omitting the
// section. The absent-versus-zero rule at the one place it decides whether
// somebody's migration step survives a deploy.
func TestClearingTheReleaseCommandIsNotTheSameAsOmittingIt(t *testing.T) {
	t.Run("explicit empty clears", func(t *testing.T) {
		apps := newFakeApps()
		apps.add("team-a", app.App{Name: "web", ReleaseCommand: "./migrate"})
		h, _ := testServer(t, apps, nil)

		w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
			`{"spec":{"name":"web","deploy":{"release_command":""}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		got, ok := apps.releases["web"]
		if !ok {
			t.Fatal("an explicit empty release_command did not reach the service")
		}
		if got != "" {
			t.Errorf("release command = %q, want cleared", got)
		}
	})

	t.Run("omitted leaves it alone", func(t *testing.T) {
		apps := newFakeApps()
		apps.add("team-a", app.App{Name: "web", ReleaseCommand: "./migrate"})
		h, _ := testServer(t, apps, nil)

		w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
			`{"spec":{"name":"web"}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if _, ok := apps.releases["web"]; ok {
			t.Error("a file that says nothing about [deploy] cleared the " +
				"release command — somebody's migration step would vanish on " +
				"the next deploy from a file that predates it")
		}
	})
}

// A build change must not reach any service method.
func TestBuildChangesReachNothing(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Image: "nginx:1", Port: 80})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","build":{"image":"nginx:2"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(apps.services) != 0 || len(apps.redeployed) != 0 {
		t.Errorf("a deferred build change touched the app: services=%v redeployed=%v",
			apps.services, apps.redeployed)
	}
	if apps.byOwner["team-a"]["web"].Image != "nginx:1" {
		t.Error("the image changed during a config converge")
	}
}

// An install with no networking surface must report a domain as unavailable,
// not as a change apply will make — AddDomain would be a nil dispatch.
//
// This path does not run on a normally-wired install, which is exactly why it
// needs a test: it is the one branch nobody exercises by using the product.
func TestDomainsOnAnInstallWithNoNetworkingAreReportedNotApplied(t *testing.T) {
	a := app.App{Name: "web"}
	spec := mustSpec(t, "name = \"web\"\n[[domains]]\nhost = \"new.example.com\"\n")

	res := plan(a, spec, nil, false, false /* canAddDomains */)

	c, ok := find(res, "domains")
	if !ok {
		t.Fatal("the domain was not reported at all")
	}
	if !c.Skipped {
		t.Fatal("reported as a change apply will make, but this install has no " +
			"AddDomain to call — apply would dispatch on a nil interface")
	}
	if c.Reason == "" {
		t.Error("skipped with no reason")
	}

	// With networking wired it is an ordinary additive change.
	withNets := plan(a, spec, nil, false, true)
	if c, _ := find(withNets, "domains"); c.Skipped {
		t.Error("a wired install refused to add a domain")
	}
}

// The same, through the endpoint: a nil Nets must not panic.
func TestConfigPutWithNoNetworkingDoesNotPanic(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})

	ident := &tokenIdentity{
		tokens: map[string]string{"oz_team-a-token": "team-a"},
		roles:  map[string]account.Role{"oz_team-a-token": account.RoleAdmin},
	}
	srv, err := New(Options{
		Identity: ident, Apps: apps, Roles: ident, Logger: quiet(), // Nets nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","domains":[{"host":"a.example.com"}]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var res ConfigResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, ok := find(res, "domains")
	if !ok || !c.Skipped {
		t.Errorf("domain change = %+v ok=%v, want skipped", c, ok)
	}
}

// apply must act on the plan, including its skips. A converge that recomputed
// what to do from the spec could apply something the preview said it would not.
func TestApplyHonoursASkippedChange(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Replicas: 10})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","scale":{"replicas":2}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(apps.scaled) != 0 {
		t.Errorf("a skipped change was applied: %v", apps.scaled)
	}

	// With scale:true it goes through.
	w = do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","scale":{"replicas":2}},"scale":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if apps.scaled["web"] != 2 {
		t.Errorf("scaled = %v, want web:2", apps.scaled)
	}
}

// --- domains ---

// Additive: named-but-missing is added, present-but-unnamed is left and
// reported. Deleting a line from a config file must not drop a certificate.
func TestDomainsAreAdditiveAndUntrackedOnesAreReported(t *testing.T) {
	a := app.App{Name: "web"}
	hosts := []string{"kept.example.com", "also-kept.example.com"}
	spec := mustSpec(t, "name = \"web\"\n[[domains]]\nhost = \"new.example.com\"\n")

	res := plan(a, spec, hosts, false, true)

	c, ok := find(res, "domains")
	if !ok || c.To != "new.example.com" {
		t.Errorf("the file's domain was not added: %+v ok=%v", c, ok)
	}

	got := append([]string{}, res.UntrackedDomains...)
	sort.Strings(got)
	want := []string{"also-kept.example.com", "kept.example.com"}
	if len(got) != len(want) {
		t.Fatalf("untracked = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("untracked = %v, want %v", got, want)
		}
	}

	// And crucially: nothing proposes to remove them.
	for _, ch := range res.Changes {
		if ch.Field == "domains" && ch.To == "" {
			t.Errorf("a converge proposed removing a domain: %+v", ch)
		}
	}
}

func TestADomainAlreadyPresentIsNotReAdded(t *testing.T) {
	a := app.App{Name: "web"}
	spec := mustSpec(t, "name = \"web\"\n[[domains]]\nhost = \"a.example.com\"\n")

	res := plan(a, spec, []string{"a.example.com"}, false, true)
	if c, ok := find(res, "domains"); ok {
		t.Errorf("re-added an existing domain: %+v", c)
	}
	if len(res.UntrackedDomains) != 0 {
		t.Errorf("untracked = %v, want none", res.UntrackedDomains)
	}
}

// --- dry run ---

func TestDryRunChangesNothing(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web", Port: 3000, Replicas: 5,
		Variables: []app.Variable{{Key: "OLD", Value: "1"}}})
	h, _ := testServer(t, apps, nil)

	body := `{"spec":{"name":"web","service":{"port":8080},"env":{"NEW":"2"}}}`
	w := do(h, http.MethodPut, "/api/v1/apps/web/config?dry_run=true", "oz_team-a-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var res ConfigResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.DryRun {
		t.Error("dry_run was not reported in the result — a client could read " +
			"a preview as a result")
	}
	if len(res.Changes) == 0 {
		t.Error("the preview listed no changes")
	}

	// Nothing was touched.
	if len(apps.vars) != 0 || len(apps.deletedVars) != 0 {
		t.Errorf("a dry run wrote variables: set=%v deleted=%v", apps.vars, apps.deletedVars)
	}
	if len(apps.scaled) != 0 {
		t.Errorf("a dry run scaled: %v", apps.scaled)
	}
	if len(apps.health) != 0 {
		t.Errorf("a dry run set health: %v", apps.health)
	}
}

// The preview and the real run must produce the same plan — the preview's
// entire value is that it does not lie about what the converge will do.
func TestDryRunAndRealRunAgree(t *testing.T) {
	apps := newFakeApps()
	seed := app.App{Name: "web", Port: 3000, Replicas: 5,
		Variables: []app.Variable{{Key: "OLD", Value: "1"}}}
	apps.add("team-a", seed)
	h, _ := testServer(t, apps, nil)

	body := `{"spec":{"name":"web","service":{"port":8080},"env":{"NEW":"2"}}}`

	dry := do(h, http.MethodPut, "/api/v1/apps/web/config?dry_run=true", "oz_team-a-token", body)
	real := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token", body)

	var a, b ConfigResult
	if err := json.Unmarshal(dry.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode dry: %v", err)
	}
	if err := json.Unmarshal(real.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode real: %v", err)
	}

	if len(a.Changes) != len(b.Changes) {
		t.Fatalf("preview listed %d changes, the run made %d", len(a.Changes), len(b.Changes))
	}
	for i := range a.Changes {
		if a.Changes[i] != b.Changes[i] {
			t.Errorf("change %d: preview %+v, run %+v", i, a.Changes[i], b.Changes[i])
		}
	}
	if a.DryRun == b.DryRun {
		t.Error("both runs reported the same dry_run flag")
	}
}

// --- round trip ---

// What configGet returns, written to a file and sent back, must be a no-op.
// That is what makes `oz config show > ozymandis.toml` safe.
func TestConfigGetRoundTripsToNoChanges(t *testing.T) {
	a := app.App{
		Name: "web", Image: "nginx:alpine", Port: 8080, Replicas: 3,
		HealthPath: "/healthz", Liveness: true, Internal: false,
		Variables: []app.Variable{{Key: "LOG_LEVEL", Value: "info"}},
	}

	spec := specFor(a)

	// Through TOML and back, the way the CLI would carry it.
	encoded, err := appspec.Encode(spec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := appspec.Parse(encoded)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, encoded)
	}

	res := plan(a, back, nil, true, true)
	if len(res.Changes) != 0 {
		t.Errorf("a spec rendered from an app proposed %d changes against that "+
			"same app: %+v\n%s", len(res.Changes), res.Changes, encoded)
	}
}

// The rendered spec must not carry a sealed value, because there is nothing to
// carry — but this pins it, since specFor reads the variable list directly.
func TestConfigGetOmitsSecrets(t *testing.T) {
	a := app.App{Name: "web", Variables: []app.Variable{
		{Key: "DATABASE_URL", Value: "", Secret: true},
		{Key: "LOG_LEVEL", Value: "info", Secret: false},
	}}
	spec := specFor(a)

	if _, ok := spec.Env["DATABASE_URL"]; ok {
		t.Error("a sealed variable appeared in the rendered spec")
	}
	if spec.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env = %v", spec.Env)
	}
}

// --- validation and tenancy on the endpoint ---

func TestConfigPutValidatesTheSpec(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_team-a-token",
		`{"spec":{"name":"web","service":{"port":0}}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestConfigIsScopedByOwner(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-b", app.App{Name: "victim"})
	h, _ := testServer(t, apps, nil)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/apps/victim/config", ""},
		{http.MethodPut, "/api/v1/apps/victim/config", `{"spec":{"name":"victim"}}`},
	} {
		w := do(h, tc.method, tc.path, "oz_team-a-token", tc.body)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", tc.method, w.Code)
		}
	}
}

func TestConfigPutIsAdminOnly(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil)

	w := do(h, http.MethodPut, "/api/v1/apps/web/config", "oz_member-token",
		`{"spec":{"name":"web"}}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}

	// Reading it is fine for a member.
	if w := do(h, http.MethodGet, "/api/v1/apps/web/config", "oz_member-token", ""); w.Code != 200 {
		t.Errorf("member reading config: status = %d, want 200", w.Code)
	}
}
