// Ingress rendering.
//
// WHAT THESE TESTS PROVE, AND WHAT THEY DO NOT.
//
// They prove EMISSION: that this package writes the annotation it means to,
// with the resolver name it was given, and no secretName. That is worth
// guarding — it catches the annotation being dropped, misspelled, or a
// secretName creeping back.
//
// They do NOT prove a certificate is ever issued, and no test in this file can.
// The predecessor of this file asserted cert-manager annotations with exactly
// this rigour, was green on every run, and was asserting strings that were
// INERT on the cluster they shipped to — there was no cert-manager there. A
// green suite meant nothing about whether TLS worked, and swapping the asserted
// string to Traefik's vocabulary does not by itself change that. The failure
// these tests cannot see is "the annotation is perfect and names a resolver the
// controller does not have", which fails silently: Ingress accepted, deploy
// green, every visitor served the controller's own certificate.
//
// The ONLY proof of the cert path is the served certificate, against a real
// cluster, with a real app:
//
//	echo | openssl s_client -connect <host>:443 -servername <host> 2>/dev/null \
//	  | openssl x509 -noout -issuer
//
// It must say Let's Encrypt. If it says TRAEFIK DEFAULT CERT, the resolver name
// is wrong and everything in this file still passes. Tests assert emission; the
// cluster proves issuance. They are not substitutes for one another.
package k8s

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

func TestApplyAppCreatesIngressForHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "web.apps.example.com" {
		t.Fatalf("rules = %+v, want one rule for web.apps.example.com", ing.Spec.Rules)
	}

	// The backend is the Service's stable port, not the container port. That
	// indirection is why changing an app's port does not rewrite its routing.
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != spec.Name || backend.Port.Number != servicePort {
		t.Fatalf("backend = %s:%d, want %s:%d",
			backend.Name, backend.Port.Number, spec.Name, servicePort)
	}

	if ing.Spec.IngressClassName != nil {
		t.Fatalf("ingressClassName = %q, want unset so the cluster default applies",
			*ing.Spec.IngressClassName)
	}
}

func TestApplyAppCreatesNoIngressWithoutHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses("ozymandis-demo").
		Get(ctx, "web", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// Every routed hostname is issued for, and the Ingress says so exactly once.
//
// Both hosts land in ONE TLS block with no secretName, because Traefik's ACME
// resolver keeps what it issues in its own store rather than in a Kubernetes
// Secret. The predecessor of this test asserted the opposite — a second block
// naming <app>-tls — which was right for cert-manager and is the silent-
// fallback bug here: Traefik looks for a Secret nothing creates, does not find
// it, and serves its built-in certificate with everything still green.
func TestIngressAsksTheResolverForEveryRoutedHost(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Issuer = orchestrator.IssuerRef{Name: "letsencrypt"}
	spec.Hosts = []orchestrator.HostSpec{
		{Name: "web.apps.example.com", Cert: orchestrator.CertIssued},
		{Name: "shop.customer.test", Cert: orchestrator.CertIssued},
	}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if got := ing.Annotations[traefikCertResolver]; got != "letsencrypt" {
		t.Fatalf("%s = %q, want letsencrypt", traefikCertResolver, got)
	}

	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("tls = %+v, want exactly one block — there is no second "+
			"certificate source to put anything in", ing.Spec.TLS)
	}
	if got := ing.Spec.TLS[0].SecretName; got != "" {
		t.Fatalf("secretName = %q, want empty — Traefik keeps the certificate in "+
			"acme.json, so a named Secret is one nothing ever creates and the "+
			"host falls back to the controller's own certificate", got)
	}
	if len(ing.Spec.TLS[0].Hosts) != 2 {
		t.Fatalf("tls hosts = %v, want both routed names", ing.Spec.TLS[0].Hosts)
	}
}

// A platform hostname reaches the resolver, at the render.
//
// The service-layer probe proves the DECISION (a managed host resolves to
// CertIssued); this proves what that decision RENDERS. Both are needed because
// they fail independently: the decision could be right and the Ingress still
// carry a secretName, which is the shape that fails silently.
//
// A managed host specifically, because it is the one that had no path here at
// all — the old selection served it from a wildcard and never reached issuance.
func TestIngressIssuesForAManagedHostWithNoSecretName(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Issuer = orchestrator.IssuerRef{Name: "letsencrypt"}
	spec.Hosts = []orchestrator.HostSpec{
		{Name: "web.apps.example.com", Cert: orchestrator.CertIssued},
	}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if got := ing.Annotations[traefikCertResolver]; got != "letsencrypt" {
		t.Fatalf("%s = %q, want letsencrypt on a platform hostname", traefikCertResolver, got)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != "web.apps.example.com" {
		t.Fatalf("tls = %+v, want one block for the managed host", ing.Spec.TLS)
	}
	if got := ing.Spec.TLS[0].SecretName; got != "" {
		t.Fatalf("secretName = %q, want none", got)
	}
}

// The resolver name is carried through, not hardcoded. An install whose
// controller calls its resolver something else must get that something else.
func TestIngressUsesTheConfiguredResolverName(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Issuer = orchestrator.IssuerRef{Name: "corporate-ca"}
	spec.Hosts = []orchestrator.HostSpec{
		{Name: "web.apps.example.com", Cert: orchestrator.CertIssued},
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if got := ing.Annotations[traefikCertResolver]; got != "corporate-ca" {
		t.Fatalf("%s = %q, want corporate-ca", traefikCertResolver, got)
	}
}

// No issued host, no resolver annotation.
//
// Not a port of the old wildcard-rate-limit test — that premise is gone with
// wildcards. This guards the live condition in ingressAnnotations: an app routed
// on plain HTTP must not name a resolver, or an install running without TLS
// carries configuration for something it is not doing.
func TestIngressAsksForNoIssuanceWhenNoHostNeedsIt(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Issuer = orchestrator.IssuerRef{Name: "letsencrypt"}
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if _, ok := ing.Annotations[traefikCertResolver]; ok {
		t.Fatal("a resolver was named for an app with no host needing a certificate")
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("tls = %+v, want none", ing.Spec.TLS)
	}
}

// HTTPS-only routing, which nothing guarded until now.
//
// This annotation was already correct for the cluster before any of the cert
// work, which is exactly why it was worth adding a test for: the parts that
// already work are the parts nobody notices breaking. Set, a request arriving
// on plain HTTP is not served at all rather than served and redirected.
func TestIngressRoutesHTTPSOnlyThroughTheSecureEntrypoint(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.HTTPSOnly = true
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if got := ing.Annotations[traefikEntrypoints]; got != "websecure" {
		t.Fatalf("%s = %q, want websecure", traefikEntrypoints, got)
	}
}

// The other half, and the half that actually bites. An annotation emitted
// unconditionally would pass the test above and silently take every app off
// plain HTTP — including apps on an install with no certificates at all, which
// would then answer nothing on :80 and nothing trusted on :443.
func TestIngressWithoutHTTPSOnlyNamesNoEntrypoint(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if got, ok := ing.Annotations[traefikEntrypoints]; ok {
		t.Fatalf("%s = %q on an app that did not ask for HTTPS-only routing",
			traefikEntrypoints, got)
	}
}

func TestApplyAppWithoutTLSEmitsNoTLSBlock(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("tls = %+v, want none", ing.Spec.TLS)
	}
}

// Removing the last hostname must remove the Ingress. Converging only forward
// leaves an app routable at a name it no longer owns.
func TestApplyAppPrunesIngressWhenHostsGoAway(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp without hosts: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

// Clearing the port already skipped Service creation but left any existing
// Service behind. Same class of bug as the Ingress one, fixed at the same time.
func TestApplyAppPrunesServiceAndIngressWhenPortCleared(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Port = 0
	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp with no port: %v", err)
	}

	if _, err := client.CoreV1().Services(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the service pruned, got %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

func TestDeleteAppRemovesIngress(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []orchestrator.HostSpec{orchestrator.Host("web.apps.example.com")}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestDeleteAppIsIdempotentWithNoIngress(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	// Deleting an app that never had an Ingress must not error, or a delete
	// after a partial apply becomes unretryable.
	if err := o.DeleteApp(ctx, testSpec().Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}
