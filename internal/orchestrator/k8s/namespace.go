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
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// limitRangeName is the default LimitRange created in every namespace.
const limitRangeName = "ozymandis-defaults"

// networkPolicyName is the isolation policy created in every app namespace.
const networkPolicyName = "ozymandis-isolation"

// The in-cluster address ranges egress is kept out of, so a workload reaches
// the internet and its own owner's pods but not another tenant's Service, the
// API server, or a neighbour's pod by raw address.
//
// These are the K3s defaults, and they match this cluster. An install that
// changed `--cluster-cidr` / `--service-cidr` would change these to match;
// they are constants rather than config because getting them wrong fails open
// — too wide a range blocks legitimate egress, too narrow leaks the thing this
// exists to contain — and that is a decision to make deliberately in a diff,
// not from an environment variable nobody will revisit.
const (
	clusterPodCIDR     = "10.42.0.0/16"
	clusterServiceCIDR = "10.43.0.0/16"
)

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
// Egress is restricted, not left open. An app still reaches the whole internet
// — an LLM API, a managed database, a payment provider — so that whole class of
// outbound call keeps working with no change. What it may no longer do is open
// a connection to another tenant's Service or pod, or to the Kubernetes API, by
// raw in-cluster address: those ranges are carved out of the internet rule.
// Without that, the ingress rule was a front-door lock on a building whose back
// wall was open — a pod could reach any other pod's port directly, and app auth
// never saw the request. Three things stay reachable on purpose: DNS (or
// nothing resolves), the owner's own namespaces (an app calling its own API or
// worker), and everything outside the cluster.
//
// One range this cannot carve out from here is the node's own address, where
// the control plane may be listening. That belongs to a host-level control —
// binding Ozymandis to the loopback or the private network and a firewall rule
// — because a node IP is not knowable, or stable, from inside a namespace spec.
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
			WithPolicyTypes(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress).
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
				)).
			WithEgress(
				dnsEgressRule(),
				// The owner's own namespaces, so an app can still reach its own
				// backend or worker — the egress counterpart of the ingress
				// rule above, and what keeps a same-owner Service call working
				// once the internet rule carves the cluster ranges out.
				networkingv1ac.NetworkPolicyEgressRule().
					WithTo(networkingv1ac.NetworkPolicyPeer().
						WithNamespaceSelector(metav1ac.LabelSelector().
							WithMatchLabels(map[string]string{
								orchestrator.LabelOwner: string(spec.Owner),
							}))),
				internetEgressRule(),
			))

	if _, err := o.client.NetworkingV1().NetworkPolicies(spec.Name).
		Apply(ctx, policy, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply network policy in %q: %w", spec.Name, err)
	}
	return nil
}

// dnsEgressRule allows name resolution, which every other egress rule depends
// on: a policy that forgets it turns every outbound call into a silent timeout
// rather than a refusal. It names the cluster DNS pods by their standard label
// rather than an address, so it holds whatever ClusterIP kube-dns was given.
func dnsEgressRule() *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(networkingv1ac.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{
					"kubernetes.io/metadata.name": "kube-system",
				})).
			WithPodSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"k8s-app": "kube-dns"}))).
		WithPorts(
			networkingv1ac.NetworkPolicyPort().
				WithProtocol(corev1.ProtocolUDP).WithPort(intstr.FromInt32(53)),
			networkingv1ac.NetworkPolicyPort().
				WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt32(53)),
		)
}

// internetEgressRule allows everything outside the cluster and nothing inside
// it: 0.0.0.0/0 with the pod and Service ranges carved out. That carve-out is
// the whole point — it is what stops a pod dialling another tenant's Service or
// the API server directly, while leaving genuine outbound calls untouched.
func internetEgressRule() *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(networkingv1ac.NetworkPolicyPeer().
			WithIPBlock(networkingv1ac.IPBlock().
				WithCIDR("0.0.0.0/0").
				WithExcept(clusterPodCIDR, clusterServiceCIDR)))
}
