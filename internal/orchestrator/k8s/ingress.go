package k8s

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Annotations the ingress controller and the DNS controller read.
//
// Both are other people's software, named by string. They are written only when
// the corresponding setting is on, so an install running neither controller
// gets an Ingress with no annotations at all rather than configuration for
// something that is not there.
const (
	// externalDNSTarget makes ExternalDNS publish a CNAME to this value instead
	// of A records for the nodes. Without it a cluster exposes its own node
	// addresses in public DNS, which is both a disclosure and a promise that
	// those addresses will not change.
	externalDNSTarget = "external-dns.alpha.kubernetes.io/target"

	// traefikEntrypoints limits which of Traefik's entrypoints will route this
	// Ingress. Set to the secure one, a request arriving on plain HTTP is not
	// served rather than served and redirected.
	traefikEntrypoints = "traefik.ingress.kubernetes.io/router.entrypoints"

	// traefikCertResolver names the ACME resolver Traefik obtains this
	// Ingress's certificate from — an entry under certificatesResolvers in
	// Traefik's own static configuration.
	//
	// Traefik keeps what it issues in its ACME store, not in a Kubernetes
	// Secret, which is why an Ingress carrying this annotation names no
	// secretName. See ingressTLS.
	traefikCertResolver = "traefik.ingress.kubernetes.io/router.tls.certresolver"
)

// ingressAnnotations returns what the spec asks the controllers for.
func ingressAnnotations(spec orchestrator.AppSpec) map[string]string {
	ann := map[string]string{}
	if spec.CNAMETarget != "" {
		ann[externalDNSTarget] = spec.CNAMETarget
	}
	if spec.HTTPSOnly {
		ann[traefikEntrypoints] = "websecure"
	}

	// Only when something on this Ingress actually needs issuing. An app with
	// no routed hostname, or an install with no resolver, gets no annotation
	// rather than one naming a resolver that will never be asked for anything.
	if len(spec.IssuedHosts()) > 0 && spec.Issuer.Set() {
		ann[traefikCertResolver] = spec.Issuer.Name
	}
	return ann
}

// ingressTLS returns the TLS block for the hostnames the resolver issues for.
//
// One block, and deliberately with NO secretName.
//
// A secretName is how an Ingress points at a certificate somebody else put in
// this namespace — the shape cert-manager needs, because it creates that Secret
// in response to the issuer annotation. Traefik's ACME resolver does not work
// that way: it keeps what it issues in its own store (acme.json on the
// controller's volume) and serves it directly.
//
// So naming a Secret here would be worse than redundant. Traefik would look for
// a Secret nothing ever creates, find it missing, and fall back to its built-in
// self-signed certificate — while the Ingress, the pod and the deploy all stay
// green. That is the whole failure this package was changed to remove, and it
// would arrive by way of a leftover field rather than a missing one.
func ingressTLS(spec orchestrator.AppSpec) []*networkingv1ac.IngressTLSApplyConfiguration {
	hosts := spec.IssuedHosts()
	if len(hosts) == 0 {
		return nil
	}
	return []*networkingv1ac.IngressTLSApplyConfiguration{
		networkingv1ac.IngressTLS().WithHosts(hosts...),
	}
}

// applyIngress routes the spec's hostnames to its Service.
//
// ingressClassName is left unset so the cluster's default IngressClass
// applies. Naming a class here would hard-code which controller is installed,
// which is the coupling this design otherwise avoids.
func (o *Orchestrator) applyIngress(ctx context.Context, spec orchestrator.AppSpec) error {
	pathType := networkingv1.PathTypePrefix

	rules := make([]*networkingv1ac.IngressRuleApplyConfiguration, 0, len(spec.Hosts))
	for _, host := range spec.Hosts {
		rules = append(rules, networkingv1ac.IngressRule().
			WithHost(host.Name).
			WithHTTP(networkingv1ac.HTTPIngressRuleValue().
				WithPaths(networkingv1ac.HTTPIngressPath().
					WithPath("/").
					WithPathType(pathType).
					WithBackend(networkingv1ac.IngressBackend().
						WithService(networkingv1ac.IngressServiceBackend().
							WithName(spec.Name).
							WithPort(networkingv1ac.ServiceBackendPort().
								WithNumber(servicePort)))))))
	}

	ingSpec := networkingv1ac.IngressSpec().WithRules(rules...)

	if tls := ingressTLS(spec); len(tls) > 0 {
		ingSpec = ingSpec.WithTLS(tls...)
	}

	ing := networkingv1ac.Ingress(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(ingSpec)

	if ann := ingressAnnotations(spec); len(ann) > 0 {
		ing = ing.WithAnnotations(ann)
	}

	if _, err := o.client.NetworkingV1().Ingresses(spec.Namespace).
		Apply(ctx, ing, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply ingress %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteIngress removes an app's Ingress, tolerating its absence.
func (o *Orchestrator) deleteIngress(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.NetworkingV1().Ingresses(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete ingress %s: %w", ref, err)
	}
	return nil
}

// deleteService removes an app's Service, tolerating its absence.
//
// Needed because clearing an app's port stops the Service being applied but
// does not remove one already there. Converging only forward leaves the old
// object serving traffic nobody asked for.
func (o *Orchestrator) deleteService(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.CoreV1().Services(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete service %s: %w", ref, err)
	}
	return nil
}
