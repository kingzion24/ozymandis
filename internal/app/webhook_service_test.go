package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/secret"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// fakeImages is enough of a registry for a git-sourced app to be created.
//
// A repository app is refused without one — there would be nowhere to push the
// image — and these tests are about webhook selection, not about builds. The
// build itself never runs here: Redeploy backgrounds it and the noop
// orchestrator has no builder.
type fakeImages struct{}

func (fakeImages) ImageFor(_ context.Context, ownerID, app, rev string) (string, error) {
	return "registry.test/" + ownerID + "/" + app + ":" + rev, nil
}
func (fakeImages) Configured(context.Context) bool              { return true }
func (fakeImages) Insecure(context.Context) bool                { return false }
func (fakeImages) DockerConfig(context.Context) ([]byte, error) { return []byte("{}"), nil }

// fakeBuilder never actually builds.
//
// Present only so CanBuild is true and a git-sourced app can be created. These
// tests assert which app a delivery selects and whether it deploys — the build
// that a deploy backgrounds is somebody else's test.
type fakeBuilder struct{}

func (fakeBuilder) Build(context.Context, orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	return orchestrator.BuildResult{CommitSHA: "abc123"}, nil
}
func (fakeBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-test" }
func (fakeBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}

// webhookService wires a service with a keeper, since webhook secrets are
// sealed and a service without one cannot hold them.
func webhookService(t *testing.T) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	keeper, err := secret.NewKeeper("0wS4iJqiUG3DQ3f5Pew+7Za6uxjBhuRkYHI7TvQ4e10=")
	if err != nil {
		t.Fatalf("keeper: %v", err)
	}
	s, _, pool := testService(t, Options{Keeper: keeper, Images: fakeImages{}, Builder: fakeBuilder{}})
	ownerID := owner(t, s, pool, "owner-webhook")
	return s, pool, ownerID
}

// autoDeployApp creates a repo-backed app with auto-deploy on and a sealed
// webhook secret, and returns the raw secret.
func autoDeployApp(
	t *testing.T, s *Service, ownerID, name, subdir, rawSecret string,
) App {
	t.Helper()
	ctx := context.Background()

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: name, Source: SourceGit, Port: 80, Replicas: 1,
		Repo: Repo{URL: "https://github.com/you/mono.git", Branch: "main", Subdir: subdir},
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}

	sealed, err := s.keeper.Seal(rawSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.q.SetAppWebhookSecret(ctx, dbgen.SetAppWebhookSecretParams{
		OwnerID: ownerID, ID: a.ID, WebhookSecret: sealed,
	}); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	if _, err := s.q.SetAppAutoDeploy(ctx, dbgen.SetAppAutoDeployParams{
		OwnerID: ownerID, ID: a.ID, AutoDeploy: true,
	}); err != nil {
		t.Fatalf("set auto-deploy: %v", err)
	}

	fresh, err := s.Get(ctx, ownerID, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return fresh
}

func pushBody(t *testing.T, sha string, paths ...string) []byte {
	t.Helper()
	ev := map[string]any{
		"ref":   "refs/heads/main",
		"after": sha,
		"repository": map[string]string{
			"full_name": "you/mono",
			"clone_url": "https://github.com/you/mono.git",
		},
		"commits": []map[string]any{{"modified": paths}},
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// THE handler-level probe, and the one that is not tautological.
//
// The unit test that app A's signature does not verify under app B's secret is
// nearly true by construction — HMAC is keyed. The real question is what the
// HANDLER does with two candidates: does it act on the app whose secret
// verified, or on the app the PAYLOAD names?
//
// The payload is attacker-controlled. Anybody can POST a body naming any
// repository, so an implementation that selects by repository URL and then
// checks the signature of whatever it found is one where the choice of app was
// already made by the attacker. Here both apps track the SAME repository — the
// monorepo case, where URL selection cannot disambiguate at all — and only the
// signature can say which delivery this is.
func TestTheSignatureSelectsTheAppNotThePayload(t *testing.T) {
	s, _, ownerID := webhookService(t)

	// Two apps, same repository, different subdirs and different secrets.
	_ = autoDeployApp(t, s, ownerID, "api", "services/api", "secret-for-api")
	_ = autoDeployApp(t, s, ownerID, "web", "services/web", "secret-for-web")

	// A push touching BOTH subdirs, so the fan-out filter cannot be what
	// distinguishes them — only the signature can.
	body := pushBody(t, "abc123", "services/api/x.go", "services/web/y.go")

	res, err := s.HandlePush(context.Background(), body, sign([]byte("secret-for-web"), body), "")
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	if res.AppName != "web" {
		t.Fatalf("acted on %q, want web — the app is chosen by whose secret "+
			"verified, not by anything the payload said", res.AppName)
	}
	if !res.Deployed {
		t.Errorf("the matched app was not deployed: %s", res.Reason)
	}
}

// The delivery must not be filtered by the payload's repository URL.
//
// This is the test the previous one could not be. Pre-filtering candidates by
// URL and then checking the signature does NOT grant access — the signature
// still runs, so an attacker gains nothing — which is why a two-apps-one-repo
// test passes against it. The damage is the other direction: it silently DROPS
// legitimate deliveries whenever the URL in the payload is not byte-identical
// to the one stored on the app.
//
// And the forms genuinely differ. GitHub sends clone_url and ssh_url; an app
// cloning over SSH stores git@github.com:you/mono.git while the payload's
// clone_url is https://github.com/you/mono.git. Neither is wrong, they are the
// same repository, and a string comparison says otherwise — so auto-deploy
// stops with no error anywhere, which is the failure mode this whole stage is
// most careful about.
func TestADeliveryIsNotFilteredByTheRepositoryURL(t *testing.T) {
	s, _, ownerID := webhookService(t)
	autoDeployApp(t, s, ownerID, "api", "", "secret-for-api")

	// The payload names the SSH form; the app stores the HTTPS one.
	ev := map[string]any{
		"ref":   "refs/heads/main",
		"after": "abc123",
		"repository": map[string]string{
			"full_name": "you/mono",
			"clone_url": "git@github.com:you/mono.git",
			"ssh_url":   "git@github.com:you/mono.git",
		},
		"commits": []map[string]any{{"modified": []string{"x.go"}}},
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	res, err := s.HandlePush(
		context.Background(), body, sign([]byte("secret-for-api"), body), "")
	if err != nil {
		t.Fatalf("HandlePush: %v — a correctly signed delivery was dropped "+
			"because the URL in the payload was spelled differently", err)
	}
	if !res.Deployed {
		t.Errorf("the delivery did not deploy: %s", res.Reason)
	}
}

// A delivery nobody can sign for is refused, and refused the same way whether
// the app does not exist or the signature is wrong.
func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	s, _, ownerID := webhookService(t)
	a := autoDeployApp(t, s, ownerID, "api", "", "the-real-secret")

	body := pushBody(t, "abc123", "x.go")

	for name, sig := range map[string]string{
		"no signature":   "",
		"wrong secret":   sign([]byte("guessed"), body),
		"tampered body":  sign([]byte("the-real-secret"), []byte("different")),
		"garbage":        "sha256=not-hex",
		"missing prefix": "abcdef",
	} {
		_, err := s.HandlePush(context.Background(), body, sig, "")
		if !errors.Is(err, ErrNoMatchingApp) {
			t.Errorf("%s: err = %v, want ErrNoMatchingApp", name, err)
		}
	}

	// And nothing was deployed.
	fresh, _ := s.Get(context.Background(), ownerID, a.Name)
	if fresh.LastDeployedSHA != "" {
		t.Errorf("an unsigned delivery recorded a deploy: %q", fresh.LastDeployedSHA)
	}
}

// The per-app URL narrows candidates but does NOT authorise: a crafted id
// reaching an app it cannot sign for is still refused.
func TestThePerAppURLIsAHintNotAnAuthorisation(t *testing.T) {
	s, _, ownerID := webhookService(t)
	api := autoDeployApp(t, s, ownerID, "api", "", "secret-for-api")
	web := autoDeployApp(t, s, ownerID, "web", "", "secret-for-web")

	body := pushBody(t, "abc123", "x.go")

	// web's signature, aimed at api's endpoint.
	_, err := s.HandlePush(
		context.Background(), body, sign([]byte("secret-for-web"), body), api.ID.String())
	if !errors.Is(err, ErrNoMatchingApp) {
		t.Errorf("err = %v, want ErrNoMatchingApp — an id in the URL must not "+
			"admit a delivery signed for a different app", err)
	}

	// And the right pairing still works.
	res, err := s.HandlePush(
		context.Background(), body, sign([]byte("secret-for-web"), body), web.ID.String())
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	if res.AppName != "web" {
		t.Errorf("acted on %q, want web", res.AppName)
	}
}

// The monorepo filter, through the handler rather than the pure function: a
// push touching only the other service must leave this one alone.
func TestAHandledPushRespectsTheSubdirFilter(t *testing.T) {
	s, _, ownerID := webhookService(t)
	api := autoDeployApp(t, s, ownerID, "api", "services/api", "secret-for-api")

	body := pushBody(t, "abc123", "services/web/only.go")

	res, err := s.HandlePush(
		context.Background(), body, sign([]byte("secret-for-api"), body), "")
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	if res.Deployed {
		t.Fatal("a push touching only services/web deployed services/api")
	}
	if res.AppName != "api" {
		t.Errorf("app = %q, want api — the delivery matched, it just declined", res.AppName)
	}

	// The commit is NOT recorded, because it was not deployed — otherwise a
	// later push that DOES touch this service would look already-deployed.
	fresh, _ := s.Get(context.Background(), ownerID, api.Name)
	if fresh.LastDeployedSHA != "" {
		t.Errorf("a declined push recorded %q as deployed", fresh.LastDeployedSHA)
	}
}

// A redelivery of a commit already deployed does nothing the second time.
func TestARedeliveryDoesNotDeployTwice(t *testing.T) {
	s, _, ownerID := webhookService(t)
	autoDeployApp(t, s, ownerID, "api", "", "secret-for-api")

	body := pushBody(t, "abc123", "x.go")
	sig := sign([]byte("secret-for-api"), body)

	first, err := s.HandlePush(context.Background(), body, sig, "")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !first.Deployed {
		t.Fatalf("the first delivery did not deploy: %s", first.Reason)
	}

	second, err := s.HandlePush(context.Background(), body, sig, "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Deployed {
		t.Error("a redelivery deployed the same commit again")
	}
}

// An app with auto-deploy off is not a candidate at all, even with a valid
// signature — turning it off must actually stop deploys.
func TestAutoDeployOffMeansNoCandidate(t *testing.T) {
	s, _, ownerID := webhookService(t)
	a := autoDeployApp(t, s, ownerID, "api", "", "secret-for-api")

	if _, err := s.q.SetAppAutoDeploy(context.Background(), dbgen.SetAppAutoDeployParams{
		OwnerID: ownerID, ID: a.ID, AutoDeploy: false,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	body := pushBody(t, "abc123", "x.go")
	_, err := s.HandlePush(
		context.Background(), body, sign([]byte("secret-for-api"), body), "")
	if !errors.Is(err, ErrNoMatchingApp) {
		t.Errorf("err = %v, want ErrNoMatchingApp — auto-deploy off must stop "+
			"deliveries being acted on at all", err)
	}
}
