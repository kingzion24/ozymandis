package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"github.com/kingzion24/ozymandis/internal/secret"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

func testService(t *testing.T, opts Options) (*Service, *recordingOrchestrator, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OZYMANDIS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OZYMANDIS_TEST_DATABASE_URL to run app service tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	orch := &recordingOrchestrator{Noop: orchestrator.NewNoop()}
	return NewService(pool, orch, log, opts), orch, pool
}

// okResolver answers every lookup with the platform's target, so a test can
// get past verification and assert on what happens after it.
//
// Verification itself is tested against a resolver that says no, over in the
// domain package. What is under test here is the certificate a proven domain
// is then served with.
type okResolver struct{}

func (okResolver) LookupCNAME(context.Context, string) (string, error) {
	return testCNAMETarget, nil
}

func (okResolver) LookupHost(context.Context, string) ([]string, error) {
	return []string{"203.0.113.10"}, nil
}

const testCNAMETarget = "edge.example.com"

// withCNAMETarget records the install's ExternalDNS target, without which a
// claim cannot be verified at all — AddCustom stores the target in force at
// the moment of the claim, and an empty one is refused rather than checked.
func withCNAMETarget(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := dbgen.New(pool).SetPlatformDNS(context.Background(),
		dbgen.SetPlatformDNSParams{CnameTarget: testCNAMETarget, TxtPrefix: "extdns-"},
	); err != nil {
		t.Fatalf("set platform dns: %v", err)
	}
}

// recordingOrchestrator keeps the last spec it was asked to apply, so a test
// can assert on what crossed the seam rather than only on what was stored.
type recordingOrchestrator struct {
	*orchestrator.Noop

	mu   sync.Mutex
	last orchestrator.AppSpec
}

func (r *recordingOrchestrator) ApplyApp(ctx context.Context, spec orchestrator.AppSpec) error {
	r.mu.Lock()
	r.last = spec
	r.mu.Unlock()
	return r.Noop.ApplyApp(ctx, spec)
}

func (r *recordingOrchestrator) lastAppSpec() orchestrator.AppSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// owner registers an owner and removes it (and its apps, by cascade) after.
func owner(t *testing.T, s *Service, pool *pgxpool.Pool, id string) string {
	t.Helper()
	ctx := context.Background()

	purge := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id); err != nil {
			t.Errorf("purge owner: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	if err := s.EnsureOwner(ctx, id, "Test", ""); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	return id
}

func TestCreateAppliesToClusterAndStores(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-create")

	a, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 2, Port: 8080,
		Env: map[string]string{"LOG_LEVEL": "info"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if a.Namespace != Namespace(id, "web") {
		t.Errorf("namespace = %q, want a derived one", a.Namespace)
	}
	// Read back rather than trusting what Create returned: variables are rows
	// now, and the question is whether they were written.
	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var found bool
	for _, v := range got.Variables {
		if v.Key == "LOG_LEVEL" && v.Value == "info" && !v.Secret {
			found = true
		}
	}
	if !found {
		t.Errorf("env round-trip failed: %+v", got.Variables)
	}

	// The workload reached the cluster, not just the database.
	if got := orch.Apps(); len(got) != 1 {
		t.Fatalf("orchestrator has %d apps, want 1", len(got))
	}
	if got := orch.Namespaces(); len(got) != 1 {
		t.Errorf("orchestrator has %d namespaces, want 1", len(got))
	}

	// And it is readable back with live status attached.
	fetched, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !fetched.StatusKnown || fetched.Status.Phase != orchestrator.PhaseRunning {
		t.Errorf("status = %+v, want a known running status", fetched.Status)
	}
}

// If the cluster rejects the workload, the database row must not survive —
// otherwise the app appears in the UI while nothing runs, and nothing ever
// retries it.
func TestCreateRollsBackWhenApplyFails(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-rollback")

	s.orch = failingOrchestrator{Noop: orchestrator.NewNoop()}

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err == nil {
		t.Fatal("expected Create to fail when the cluster rejects the workload")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM apps WHERE owner_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d rows survived a failed apply, want 0", count)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-dupe")

	in := CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1}
	if _, err := s.Create(ctx, id, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create(ctx, id, in)
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("err = %v, want ErrNameTaken", err)
	}
}

// Two owners may each have an app called "web". This is the property that
// makes the schema extensible to more than one owner later.
func TestNamesAreScopedByOwner(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	a := owner(t, s, pool, "svc-owner-a")
	b := owner(t, s, pool, "svc-owner-b")

	in := CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1}
	if _, err := s.Create(ctx, a, in); err != nil {
		t.Fatalf("owner a: %v", err)
	}
	if _, err := s.Create(ctx, b, in); err != nil {
		t.Errorf("owner b must be able to use the same app name: %v", err)
	}

	// And neither can see the other's.
	list, err := s.List(ctx, a)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("owner a sees %d apps, want 1", len(list))
	}
	if _, err := s.Get(ctx, a, "web"); err != nil {
		t.Errorf("owner a should see their own app: %v", err)
	}
}

func TestGetUnknownAppIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-missing")

	if _, err := s.Get(ctx, id, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestScaleAndDelete(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-scale")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	scaled, err := s.Scale(ctx, id, "web", 4)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if scaled.Replicas != 4 {
		t.Errorf("replicas = %d, want 4", scaled.Replicas)
	}

	if err := s.Delete(ctx, id, "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(orch.Apps()) != 0 {
		t.Error("workload should be gone from the cluster")
	}
	if _, err := s.Get(ctx, id, "web"); !errors.Is(err, ErrNotFound) {
		t.Error("record should be gone from the database")
	}
}

// A cluster that cannot be reached must not turn a list into an error. The
// records are still real and the operator still needs to see them.
func TestListToleratesUnreachableCluster(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-degraded")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.orch = failingOrchestrator{Noop: orchestrator.NewNoop()}

	list, err := s.List(ctx, id)
	if err != nil {
		t.Fatalf("List should not fail when the cluster is down: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d apps, want 1", len(list))
	}
	if list[0].StatusKnown {
		t.Error("status must be reported as unknown rather than as stopped")
	}
}

func TestCreateIssuesAManagedHostname(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.example.com", CertResolver: letsencrypt})
	id := owner(t, s, pool, "svc-host")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.Host != "web.apps.example.com" {
		t.Fatalf("Host = %q, want web.apps.example.com", created.Host)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "web.apps.example.com" {
		t.Errorf("Host on read = %q, want web.apps.example.com", got.Host)
	}
	if got.URLScheme() != "https" {
		t.Errorf("URLScheme = %q, want https when a resolver is configured", got.URLScheme())
	}
}

func TestCreateWithoutAnAppDomainIssuesNoHostname(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-no-host")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Host != "" {
		t.Fatalf("Host = %q, want empty when the feature is off", created.Host)
	}
}

// Changing the app domain moves each app's URL the next time it is applied,
// rather than all at once at startup. The managed column is what makes that
// safe: the reconcile rewrites only rows Ozymandis issued.
func TestApplyReissuesWhenTheAppDomainChanges(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.example.com"})
	id := owner(t, s, pool, "svc-reissue")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.opts.AppDomain = "apps.acme.com"

	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "web.apps.acme.com" {
		t.Fatalf("Host = %q, want it reissued to web.apps.acme.com", got.Host)
	}
}

// letsencrypt is the resolver the tests configure. Named once so that the
// thing under test is "a resolver is configured", not a string literal.
var letsencrypt = orchestrator.IssuerRef{Name: "letsencrypt"}

// The branch that did not exist.
//
// A managed hostname — one the platform minted under the app domain — must
// reach CertIssued. Until this change it could not: the selection ran
// `h.Managed && WildcardTLS` first, so a platform hostname was either served
// from a wildcard the operator had to supply or served over plain HTTP, and no
// input reached the issued branch for one.
//
// That is why this asserts on a MANAGED host specifically. A test using a
// brought domain passes against the old code and the new one alike, and would
// have reported this whole area as working while every platform subdomain on a
// per-host-issuing controller was served a self-signed certificate.
func TestAManagedHostnameGetsItsOwnCertificate(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{
		AppDomain:    "apps.example.com",
		CertResolver: letsencrypt,
	})
	id := owner(t, s, pool, "svc-managed-cert")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	hosts := orch.lastAppSpec().Hosts
	if len(hosts) != 1 || hosts[0].Name != "web.apps.example.com" {
		t.Fatalf("spec.Hosts = %v, want the one managed hostname", hosts)
	}
	if hosts[0].Cert != orchestrator.CertIssued {
		t.Fatalf("managed host cert = %q, want %q — a platform hostname must be "+
			"issued for on its own name, not served from something else's certificate",
			hosts[0].Cert, orchestrator.CertIssued)
	}
}

// The other half of the same rule: no resolver, no certificate, for a managed
// host as much as a brought one. Paired with the test above so that "always
// CertIssued" cannot pass by ignoring configuration.
func TestAManagedHostnameGetsNoCertificateWithoutAResolver(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.example.com"})
	id := owner(t, s, pool, "svc-managed-nocert")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	hosts := orch.lastAppSpec().Hosts
	if len(hosts) != 1 || hosts[0].Cert != orchestrator.CertNone {
		t.Fatalf("hosts = %v, want the managed host on plain HTTP with no resolver", hosts)
	}
}

func TestApplyPassesHostsToTheOrchestrator(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.example.com", CertResolver: letsencrypt})
	id := owner(t, s, pool, "svc-host-spec")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spec := orch.lastAppSpec()
	if len(spec.Hosts) != 1 || spec.Hosts[0].Name != "web.apps.example.com" {
		t.Fatalf("spec.Hosts = %v, want [web.apps.example.com]", spec.Hosts)
	}
	if spec.Hosts[0].Cert != orchestrator.CertIssued {
		t.Fatalf("cert = %q, want issued — a platform hostname is issued for like any other",
			spec.Hosts[0].Cert)
	}
}

// Both hostnames on one Ingress get a certificate, each issued for its own
// name. Serving a brought domain under anything else was the bug: the
// connection succeeds, under a certificate issued for a name the visitor never
// asked for.
func TestApplyGivesACustomDomainItsOwnCertificate(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{
		AppDomain:    "apps.example.com",
		CertResolver: letsencrypt,
		Resolver:     okResolver{},
	})
	id := owner(t, s, pool, "svc-host-issued")
	withCNAMETarget(t, pool)

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.AddDomain(ctx, id, "web", "shop.brought.test"); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	n, err := s.Networking(ctx, id, "web")
	if err != nil {
		t.Fatalf("Networking: %v", err)
	}
	if len(n.Custom) != 1 {
		t.Fatalf("custom domains = %v, want one", n.Custom)
	}
	if err := s.VerifyDomain(ctx, id, "web", n.Custom[0].ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}

	certs := map[string]orchestrator.Certificate{}
	for _, h := range orch.lastAppSpec().Hosts {
		certs[h.Name] = h.Cert
	}
	if got := certs["web.apps.example.com"]; got != orchestrator.CertIssued {
		t.Errorf("platform host cert = %q, want one issued for it", got)
	}
	if got := certs["shop.brought.test"]; got != orchestrator.CertIssued {
		t.Errorf("brought domain cert = %q, want one issued for it", got)
	}
}

// Without a resolver the brought domain is served over plain HTTP. Deliberate:
// the alternative is a certificate issued for another name, which is a
// browser-level impersonation warning rather than a working site.
func TestACustomDomainWithNoResolverIsServedPlainHTTP(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{
		AppDomain: "apps.example.com",
		Resolver:  okResolver{},
	})
	id := owner(t, s, pool, "svc-host-noissuer")
	withCNAMETarget(t, pool)

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.AddDomain(ctx, id, "web", "shop.brought.test"); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	n, err := s.Networking(ctx, id, "web")
	if err != nil {
		t.Fatalf("Networking: %v", err)
	}
	if err := s.VerifyDomain(ctx, id, "web", n.Custom[0].ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}

	for _, h := range orch.lastAppSpec().Hosts {
		if h.Name == "shop.brought.test" && h.Cert != orchestrator.CertNone {
			t.Fatalf("brought domain cert = %q, want none — there is no resolver "+
				"to obtain one from", h.Cert)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   CreateInput
		ok   bool
	}{
		{"valid", CreateInput{Name: "web", Image: "nginx", Replicas: 1}, true},
		{"no name", CreateInput{Image: "nginx"}, false},
		{"uppercase", CreateInput{Name: "Web", Image: "nginx"}, false},
		{"underscore", CreateInput{Name: "my_app", Image: "nginx"}, false},
		{"leading dash", CreateInput{Name: "-web", Image: "nginx"}, false},
		{"path traversal", CreateInput{Name: "../etc", Image: "nginx"}, false},
		{"no image", CreateInput{Name: "web"}, false},
		{"negative replicas", CreateInput{Name: "web", Image: "nginx", Replicas: -1}, false},
		{"too many replicas", CreateInput{Name: "web", Image: "nginx", Replicas: 500}, false},
		{"bad port", CreateInput{Name: "web", Image: "nginx", Port: 70000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.ok && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

// Namespaces must be deterministic, owner-scoped, and immune to the
// truncation collision that a slug-and-trim scheme suffers from.
func TestNamespaceDerivation(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		if Namespace("o", "web") != Namespace("o", "web") {
			t.Error("must be stable for the same inputs")
		}
	})

	t.Run("differs by owner", func(t *testing.T) {
		if Namespace("a", "web") == Namespace("b", "web") {
			t.Error("two owners must not share a namespace")
		}
	})

	t.Run("long names do not collide", func(t *testing.T) {
		long1 := "a-very-long-application-name-that-would-be-truncated-aaaa"
		long2 := "a-very-long-application-name-that-would-be-truncated-bbbb"
		if Namespace("o", long1) == Namespace("o", long2) {
			t.Error("names that differ only past the truncation point must not collide")
		}
	})

	t.Run("is a legal kubernetes label", func(t *testing.T) {
		ns := Namespace("owner", "web")
		if len(ns) > 63 {
			t.Errorf("namespace %q is %d chars, over the 63 limit", ns, len(ns))
		}
		if err := orchestrator.ValidateDNSLabel("namespace", ns); err != nil {
			t.Errorf("namespace is not a valid DNS label: %v", err)
		}
	})
}

// failingOrchestrator rejects every mutation, standing in for an unreachable
// or hostile cluster.
type failingOrchestrator struct{ *orchestrator.Noop }

var errCluster = errors.New("cluster refused the request")

func (failingOrchestrator) EnsureNamespace(context.Context, orchestrator.NamespaceSpec) error {
	return errCluster
}

func (failingOrchestrator) ApplyApp(context.Context, orchestrator.AppSpec) error {
	return errCluster
}

func (failingOrchestrator) AppStatus(
	context.Context, orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	return orchestrator.AppStatus{}, errCluster
}

// TestCreatePortlessAppWithAnAppDomain is a regression test.
//
// A workload with no port takes no traffic, and the dashboard turns a blank
// port field into 0 — so if issuing a hostname were unconditional, switching
// the feature on would make every background worker uncreatable.
func TestCreatePortlessAppWithAnAppDomain(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.example.com", CertResolver: letsencrypt})
	id := owner(t, s, pool, "svc-portless")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "worker", Image: "busybox:latest", Replicas: 1, Port: 0,
	})
	if err != nil {
		t.Fatalf("a port-less app must still be creatable: %v", err)
	}
	if created.Host != "" {
		t.Errorf("Host = %q, want empty — nothing can reach a workload with no port", created.Host)
	}

	// Holding a globally unique name that no request can reach would block
	// another app from ever using it.
	if got := orch.lastAppSpec().Hosts; len(got) != 0 {
		t.Errorf("spec.Hosts = %v, want none", got)
	}
}

// TestRetiringTheAppDomainReleasesTheHostname is a regression test.
//
// Backing the feature out has to actually stop the routing. Leaving the row
// behind keeps the app served at a name the operator has retired, and makes
// the orchestrator's prune-on-empty-hosts path unreachable.
func TestRetiringTheAppDomainReleasesTheHostname(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.example.com"})
	id := owner(t, s, pool, "svc-retire")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.opts.AppDomain = ""

	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	if got := orch.lastAppSpec().Hosts; len(got) != 0 {
		t.Errorf("spec.Hosts = %v, want none so the Ingress is pruned", got)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "" {
		t.Errorf("Host = %q, want empty — the dashboard must stop offering a retired URL", got.Host)
	}
}

func TestAttachVolumeReachesTheOrchestrator(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-vol")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{
		Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	spec := orch.lastAppSpec()
	if len(spec.Volumes) != 1 || spec.Volumes[0].MountPath != "/var/lib/data" {
		t.Fatalf("spec.Volumes = %+v, want one at /var/lib/data", spec.Volumes)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Name != "data" {
		t.Fatalf("app volumes on read = %+v, want one named data", got.Volumes)
	}
}

// Kubernetes cannot shrink a claim, so neither can this. Refusing before
// anything reaches the cluster keeps the database and the cluster agreeing.
func TestVolumesGrowButNeverShrink(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-vol-grow")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{
		Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	if err := s.ResizeVolume(ctx, id, "web", "data", 2<<30); err != nil {
		t.Fatalf("growing: %v", err)
	}
	if err := s.ResizeVolume(ctx, id, "web", "data", 1<<30); !errors.Is(err, ErrVolumeShrink) {
		t.Fatalf("shrinking: want ErrVolumeShrink, got %v", err)
	}

	got, _ := s.Get(ctx, id, "web")
	if got.Volumes[0].SizeBytes != 2<<30 {
		t.Fatalf("size = %d, want the grown 2GiB", got.Volumes[0].SizeBytes)
	}
}

// A volume with a workload mounted on it is refused. Detaching is an edit of
// the app; deleting the storage is a separate act.
func TestDeletingAnAttachedVolumeIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-vol-del")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{
		Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	if err := s.DeleteVolume(ctx, id, "web", "data", false); !errors.Is(err, ErrVolumeAttached) {
		t.Fatalf("want ErrVolumeAttached, got %v", err)
	}
	// Detached first, it goes.
	if err := s.DeleteVolume(ctx, id, "web", "data", true); err != nil {
		t.Fatalf("detach and delete: %v", err)
	}
	got, _ := s.Get(ctx, id, "web")
	if len(got.Volumes) != 0 {
		t.Fatalf("volumes = %+v, want none", got.Volumes)
	}
}

// Storage forces one replica. Scaling an app that has a volume must be
// refused, not silently produce a workload that can never become ready.
func TestScalingAnAppWithStorageIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-vol-scale")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{
		Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	if _, err := s.Scale(ctx, id, "web", 3); err == nil {
		t.Fatal("scaled an app with storage to three replicas")
	}
}

// TestSecretIsUnreadableInTheDatabase is the claim the feature exists to make.
func TestSecretIsUnreadableInTheDatabase(t *testing.T) {
	ctx := context.Background()
	k, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, orch, pool := testService(t, Options{Keeper: k})
	id := owner(t, s, pool, "svc-secret")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const password = "hunter2-not-in-any-dump"
	if err := s.SetVariable(ctx, id, "web", VariableInput{
		Key: "DATABASE_URL", Value: password, Secret: true,
	}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}

	// Not in any column of the row that holds it.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM variables
		 WHERE value LIKE '%' || $1 || '%'
		    OR encode(sealed, 'escape') LIKE '%' || $1 || '%'`,
		password).Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Fatal("the secret is recoverable from the variables table")
	}

	// Not readable through the API a page uses.
	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, v := range got.Variables {
		if v.Value == password {
			t.Fatal("the secret came back through the read path a page uses")
		}
		if v.Key == "DATABASE_URL" && !v.Secret {
			t.Fatal("the secret is not marked secret")
		}
	}

	// But it does reach the cluster, as a Secret rather than a literal.
	spec := orch.lastAppSpec()
	if spec.Secrets["DATABASE_URL"] != password {
		t.Fatalf("the workload did not receive the secret: %+v", spec.Secrets)
	}
	if spec.Env["DATABASE_URL"] != "" {
		t.Fatal("the secret was also placed in the plain environment")
	}
}

// Without a key, a secret is refused rather than stored readable.
func TestSecretRefusedWithoutAKey(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-nokey")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := s.SetVariable(ctx, id, "web", VariableInput{
		Key: "TOKEN", Value: "sk_live_x", Secret: true,
	})
	if !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("want ErrNoSecretKey, got %v", err)
	}

	got, _ := s.Get(ctx, id, "web")
	if len(got.Variables) != 0 {
		t.Fatalf("a refused secret was stored anyway: %+v", got.Variables)
	}
}

// A database that arrives needing to be finished is not a source, it is a
// form with defaults. Everything it needs has to be there when it is created.
func TestPostgresSourceArrivesComplete(t *testing.T) {
	ctx := context.Background()
	k, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, orch, pool := testService(t, Options{Keeper: k, AppDomain: "apps.example.test"})
	id := owner(t, s, pool, "svc-pg")

	if _, err := s.Create(ctx, id, CreateInput{Source: SourcePostgres, Name: "db"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, id, "db")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Storage, without anybody attaching it.
	if len(got.Volumes) != 1 || got.Volumes[0].MountPath != "/var/lib/postgresql/data" {
		t.Fatalf("volumes = %+v, want one at the data directory", got.Volumes)
	}

	// Credentials nobody chose, and none of them readable.
	var hasPassword, hasConn bool
	for _, v := range got.Variables {
		switch v.Key {
		case "POSTGRES_PASSWORD":
			hasPassword = true
		case "DATABASE_URL":
			hasConn = true
		}
		if v.Key == "POSTGRES_PASSWORD" || v.Key == "DATABASE_URL" {
			if !v.Secret {
				t.Errorf("%s is not marked secret", v.Key)
			}
			if v.Value != "" {
				t.Errorf("%s came back readable", v.Key)
			}
		}
	}
	if !hasPassword || !hasConn {
		t.Fatalf("variables = %+v, want a password and a connection string", got.Variables)
	}

	// A database speaks its own protocol, so it gets no HTTP hostname.
	if got.Host != "" {
		t.Errorf("a database was given the hostname %q", got.Host)
	}

	// And the runtime facts the image needs to start at all.
	spec := orch.lastAppSpec()
	if spec.RunAsUser != 70 || spec.FSGroup != 70 {
		t.Errorf("runAsUser=%d fsGroup=%d, want 70 — the image will not start otherwise",
			spec.RunAsUser, spec.FSGroup)
	}
	if len(spec.ScratchPaths) == 0 {
		t.Error("no writable scratch path — postgres cannot open its socket")
	}
	if spec.Secrets["POSTGRES_PASSWORD"] == "" {
		t.Error("the workload did not receive its password")
	}
}

func TestRedisSourceArrivesComplete(t *testing.T) {
	ctx := context.Background()
	k, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, orch, pool := testService(t, Options{Keeper: k, AppDomain: "apps.example.test"})
	id := owner(t, s, pool, "svc-redis")

	if _, err := s.Create(ctx, id, CreateInput{Source: SourceRedis, Name: "cache"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, id, "cache")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(got.Volumes) != 1 || got.Volumes[0].MountPath != "/data" {
		t.Fatalf("volumes = %+v, want one at /data", got.Volumes)
	}

	var hasPassword, hasConn bool
	for _, v := range got.Variables {
		if v.Key == "REDIS_PASSWORD" || v.Key == "REDIS_URL" {
			hasPassword = hasPassword || v.Key == "REDIS_PASSWORD"
			hasConn = hasConn || v.Key == "REDIS_URL"
			if !v.Secret {
				t.Errorf("%s is not marked secret", v.Key)
			}
			if v.Value != "" {
				t.Errorf("%s came back readable", v.Key)
			}
		}
	}
	if !hasPassword || !hasConn {
		t.Fatalf("variables = %+v, want a password and a connection string", got.Variables)
	}

	// Redis speaks its own protocol, so no HTTP hostname.
	if got.Host != "" {
		t.Errorf("redis was given the hostname %q", got.Host)
	}

	spec := orch.lastAppSpec()

	// The image declares no USER, so without this the kubelet refuses the pod
	// outright rather than starting it as root.
	if spec.RunAsUser != 999 || spec.FSGroup != 999 {
		t.Errorf("runAsUser=%d fsGroup=%d, want 999 — the image will not start otherwise",
			spec.RunAsUser, spec.FSGroup)
	}
	if spec.Secrets["REDIS_PASSWORD"] == "" {
		t.Error("the workload did not receive its password")
	}
}

// The password must reach Redis without ever being written into the pod
// template, which is what the shell in the command is for.
func TestRedisIsPasswordProtectedWithoutLeakingIt(t *testing.T) {
	ctx := context.Background()
	k, _ := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	s, orch, pool := testService(t, Options{Keeper: k})
	id := owner(t, s, pool, "svc-redis2")

	if _, err := s.Create(ctx, id, CreateInput{Source: SourceRedis, Name: "cache"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spec := orch.lastAppSpec()
	password := spec.Secrets["REDIS_PASSWORD"]
	if password == "" {
		t.Fatal("no password was generated")
	}

	joined := strings.Join(spec.Command, " ")
	if !strings.Contains(joined, "--requirepass") {
		t.Fatalf("command does not set a password: %q", joined)
	}
	// The literal name, never the value: ParseCommand expands nothing, so the
	// shell inside the container is what resolves it.
	if !strings.Contains(joined, "$REDIS_PASSWORD") {
		t.Errorf("command does not reference the variable: %q", joined)
	}
	if strings.Contains(joined, password) {
		t.Error("the generated password was written into the pod template")
	}

	// A cache that silently drops what it is holding is worse than one that
	// refuses a write: the entries at risk are unprocessed work and the
	// counters that enforce limits.
	if !strings.Contains(joined, "--maxmemory-policy noeviction") {
		t.Errorf("eviction policy is not noeviction: %q", joined)
	}
	if !strings.Contains(joined, "--appendonly yes") {
		t.Errorf("persistence is off, so a restart loses the data: %q", joined)
	}
}

// The command has to survive ParseCommand as three arguments — a shell, -c, and
// one script — or the container runs something that is not a command line.
func TestRedisCommandParsesAsAShellInvocation(t *testing.T) {
	b, err := BlueprintFor(SourceRedis)
	if err != nil {
		t.Fatalf("BlueprintFor: %v", err)
	}
	argv, err := ParseCommand(b.Command)
	if err != nil {
		t.Fatalf("ParseCommand(%q): %v", b.Command, err)
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv = %#v, want [sh -c <script>]", argv)
	}
	// exec, so redis is PID 1 and receives the signals Kubernetes sends it —
	// without it the shell gets them and the container stops on a timeout
	// rather than on request.
	if !strings.HasPrefix(argv[2], "exec redis-server") {
		t.Errorf("script does not exec: %q", argv[2])
	}
	if !strings.Contains(argv[2], `"$REDIS_PASSWORD"`) {
		t.Errorf("the variable is not quoted in the script: %q", argv[2])
	}
}

// A blueprint command is a default. Somebody who names their own runs theirs.
func TestABlueprintCommandDoesNotOverrideAChosenOne(t *testing.T) {
	ctx := context.Background()
	k, _ := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	s, orch, pool := testService(t, Options{Keeper: k})
	id := owner(t, s, pool, "svc-redis3")

	_, err := s.Create(ctx, id, CreateInput{
		Source: SourceRedis, Name: "cache", Command: "redis-server --port 6379",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if joined := strings.Join(orch.lastAppSpec().Command, " "); joined != "redis-server --port 6379" {
		t.Errorf("command = %q, want the one that was asked for", joined)
	}
}

// Two databases must not share a password.
func TestPostgresPasswordsAreNotShared(t *testing.T) {
	ctx := context.Background()
	k, _ := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	s, orch, pool := testService(t, Options{Keeper: k})
	id := owner(t, s, pool, "svc-pg2")

	if _, err := s.Create(ctx, id, CreateInput{Source: SourcePostgres, Name: "one"}); err != nil {
		t.Fatalf("Create one: %v", err)
	}
	first := orch.lastAppSpec().Secrets["POSTGRES_PASSWORD"]

	if _, err := s.Create(ctx, id, CreateInput{Source: SourcePostgres, Name: "two"}); err != nil {
		t.Fatalf("Create two: %v", err)
	}
	second := orch.lastAppSpec().Secrets["POSTGRES_PASSWORD"]

	if first == "" || first == second {
		t.Fatal("two databases were given the same password")
	}
}

// The scheme has to match the entrypoint that actually serves the app.
//
// HTTPSOnly writes traefik.ingress.kubernetes.io/router.entrypoints=websecure
// onto the Ingress, so nothing answers on port 80. A dashboard reading the
// platform's certificate instead of that setting offers an http:// link to an
// app that 404s — which is what it did, and what somebody reports as the app
// being broken rather than as the link being wrong.
func TestTheSchemeFollowsWhatActuallyServes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		httpsOnly, tls bool
		want           string
	}{
		{"https-only without a platform certificate", true, false, "https"},
		{"https-only with one", true, true, "https"},
		{"plain http", false, false, "http"},
		// The platform serving TLS is reason enough on its own: the app is
		// reachable on both entrypoints and the secure one is the better link.
		{"platform TLS without the app enforcing it", false, true, "https"},
	} {
		a := App{Host: "web.apps.example.com", HTTPSOnly: tc.httpsOnly, TLS: tc.tls}
		if got := a.URLScheme(); got != tc.want {
			t.Errorf("%s: scheme = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An app served over https with nothing to prove it says so.
//
// The browser's warning names the certificate. It does not say the install has
// none configured, which is the only part anybody can act on.
func TestAnUntrustedCertificateIsReported(t *testing.T) {
	if !(App{Host: "web.example.com", HTTPSOnly: true}).UntrustedCert() {
		t.Error("https with no platform certificate is not reported")
	}
	if (App{Host: "web.example.com", HTTPSOnly: true, TLS: true}).UntrustedCert() {
		t.Error("a properly certificated app is reported as untrusted")
	}
	if (App{HTTPSOnly: true}).UntrustedCert() {
		t.Error("an app with no hostname is reported as untrusted")
	}
}
