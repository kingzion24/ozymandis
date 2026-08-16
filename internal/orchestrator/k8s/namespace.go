package k8s

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// limitRangeName is the default LimitRange created in every namespace.
const limitRangeName = "ozymandis-defaults"

// networkPolicyName is the isolation policy created in every app namespace.
const networkPolicyName = "ozymandis-isolation"

// EnsureNamespace creates or converges an owner's namespace.
//
// Two things always happen together here, and that coupling is intentional:
// the namespace gets Pod Security Admission labels, and it gets a LimitRange.
// A namespace without PSA labels has no enforced security floor; a namespace
// without a LimitRange lets a workload that specifies no resources run
// unbounded and evict its neighbours. Neither is offered as an option.
func (o *Orchestrator) EnsureNamespace(ctx context.Context, spec orchestrator.NamespaceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	limits := spec.Limits.OrDefaults()

	labels := psaLabels()
	labels[orchestrator.LabelManagedBy] = orchestrator.ManagedByValue
	labels[orchestrator.LabelOwner] = string(spec.Owner)

	nsAC := corev1ac.Namespace(spec.Name).WithLabels(labels)

	if _, err := o.client.CoreV1().Namespaces().
		Apply(ctx, nsAC, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply namespace %q: %w", spec.Name, err)
	}

	if err := o.ensureLimitRange(ctx, spec.Name, limits); err != nil {
		return err
	}

	if err := o.ensureNetworkPolicy(ctx, spec); err != nil {
		return err
	}

	o.log.Info("namespace ready",
		slog.String("namespace", spec.Name),
		slog.String("owner", string(spec.Owner)),
	)
	return nil
}

func (o *Orchestrator) ensureLimitRange(
	ctx context.Context, namespace string, limits orchestrator.ResourceLimits,
) error {
	defaultLimit, err := resourceList(limits.DefaultCPU, limits.DefaultMemory)
	if err != nil {
		return fmt.Errorf("k8s: default limits: %w", err)
	}
	maxLimit, err := resourceList(limits.MaxCPU, limits.MaxMemory)
	if err != nil {
		return fmt.Errorf("k8s: max limits: %w", err)
	}

	lrAC := corev1ac.LimitRange(limitRangeName, namespace).
		WithLabels(map[string]string{
			orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		}).
		WithSpec(corev1ac.LimitRangeSpec().WithLimits(
			corev1ac.LimitRangeItem().
				WithType(corev1.LimitTypeContainer).
				WithDefault(defaultLimit).
				WithDefaultRequest(defaultLimit).
				WithMax(maxLimit),
		))

	if _, err := o.client.CoreV1().LimitRanges(namespace).
		Apply(ctx, lrAC, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply limitrange in %q: %w", namespace, err)
	}
	return nil
}

// DeleteNamespace removes a namespace and everything inside it.
func (o *Orchestrator) DeleteNamespace(ctx context.Context, name string) error {
	if err := orchestrator.ValidateDNSLabel("namespace", name); err != nil {
		return err
	}
	err := o.client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete namespace %q: %w", name, err)
	}
	o.log.Info("namespace deleted", slog.String("namespace", name))
	return nil
}

// resourceList builds a Kubernetes ResourceList from quantity strings,
// skipping empty ones.
func resourceList(cpu, memory string) (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return nil, fmt.Errorf("parse cpu %q: %w", cpu, err)
		}
		out[corev1.ResourceCPU] = q
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return nil, fmt.Errorf("parse memory %q: %w", memory, err)
		}
		out[corev1.ResourceMemory] = q
	}
	return out, nil
}

// applyOpts returns the server-side apply options used everywhere.
//
// Force is on because the engine is the sole owner of the fields it sets; if
// something else has claimed one, the engine's desired state should win rather
// than the apply failing with a conflict a user cannot act on.
func applyOpts() metav1.ApplyOptions {
	return metav1.ApplyOptions{FieldManager: orchestrator.FieldManager, Force: true}
}

// ensureNetworkPolicy isolates an owner's namespace at the network layer.
//
// Without one, "internal" means only that an app has no Ingress — every pod in
// the cluster can still reach every other pod's Service and port directly. For
// this system that is the difference between a tenant boundary and a naming
// convention: an app whose front door checks a JWT and an ownership claim is
// reachable, from any other pod, at a back door that checks neither.
//
// The policy denies ingress by default and allows exactly two sources:
//
//   - namespaces belonging to the SAME OWNER, because an owner's apps are
//     wired to each other on purpose — a backend calling its own API server,
//     a worker draining its own queue. Isolating those would break the thing
//     the platform exists to run. The owner's own namespace matches this too,
//     which is what lets a pod talk to its neighbour.
//
//   - the ingress controller's namespace, or no public app is reachable at
//     all. Traefik routes to a Service in another namespace, and to the policy
//     that is ordinary cross-namespace traffic.
//
// Egress is deliberately untouched. An app legitimately calls out to the whole
// internet — an LLM API, a managed database, a payment provider — and a default
// deny there would break every one of those with a timeout rather than an
// error, which is the least debuggable failure this could produce.
//
// Skipped entirely when the install has not said where its ingress controller
// lives, because the alternative is applying a policy that silently makes every
// public app unreachable. Ozymandis does not install the edge and cannot guess
// it; a feature it cannot do correctly is off rather than half-on, which is the
// same rule the secret-key gates follow.
func (o *Orchestrator) ensureNetworkPolicy(
	ctx context.Context, spec orchestrator.NamespaceSpec,
) error {
	if o.ingressNamespace == "" {
		o.log.Warn("no ingress namespace configured, so namespaces are not network-isolated",
			slog.String("namespace", spec.Name),
			slog.String("fix", "set OZYMANDIS_INGRESS_NAMESPACE to where your ingress controller runs"))
		return nil
	}

	policy := networkingv1ac.NetworkPolicy(networkPolicyName, spec.Name).
		WithLabels(map[string]string{
			orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
			orchestrator.LabelOwner:     string(spec.Owner),
		}).
		WithSpec(networkingv1ac.NetworkPolicySpec().
			// Every pod in the namespace. An empty selector is the whole point:
			// a policy that named specific pods would leave anything added
			// later unprotected by default.
			WithPodSelector(metav1ac.LabelSelector()).
			WithPolicyTypes(networkingv1.PolicyTypeIngress).
			WithIngress(networkingv1ac.NetworkPolicyIngressRule().
				// Two peers in ONE rule is OR, not AND. As separate rules the
				// meaning would be the same, but as separate `from` entries in
				// one rule it reads as the single sentence it is: traffic from
				// this owner, or from the edge.
				WithFrom(
					networkingv1ac.NetworkPolicyPeer().
						WithNamespaceSelector(metav1ac.LabelSelector().
							WithMatchLabels(map[string]string{
								orchestrator.LabelOwner: string(spec.Owner),
							})),
					networkingv1ac.NetworkPolicyPeer().
						WithNamespaceSelector(metav1ac.LabelSelector().
							WithMatchLabels(map[string]string{
								// Set by Kubernetes on every namespace, so it
								// needs nothing labelled by hand on a
								// controller this install does not own.
								"kubernetes.io/metadata.name": o.ingressNamespace,
							})),
				)))

	if _, err := o.client.NetworkingV1().NetworkPolicies(spec.Name).
		Apply(ctx, policy, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply network policy in %q: %w", spec.Name, err)
	}
	return nil
}
