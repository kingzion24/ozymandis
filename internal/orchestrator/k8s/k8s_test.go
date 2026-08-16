package k8s

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

func testOrchestrator(t *testing.T) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWithClient(client, log), client
}

func testSpec() orchestrator.AppSpec {
	return orchestrator.AppSpec{
		Ref: orchestrator.Ref{
			Owner:     "owner-1",
			Namespace: "ozymandis-demo",
			Name:      "web",
		},
		Image:    "ghcr.io/example/web:v1",
		Replicas: 2,
		Port:     8080,
		Env:      map[string]string{"LOG_LEVEL": "info", "APP_ENV": "prod"},
	}
}

// TestEnsureNamespace is half of the smallest loop: a namespace that is safe by
// construction. The assertions here are the security posture — if any of them
// regress, workloads lose their floor.
func TestEnsureNamespace(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-demo"}
	if err := o.EnsureNamespace(ctx, spec); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}

	ns, err := client.CoreV1().Namespaces().Get(ctx, "ozymandis-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	// Pod Security Admission is what the cluster enforces, independent of what
	// the control plane chooses to send.
	for key, want := range map[string]string{
		psaEnforce:                  "restricted",
		psaAudit:                    "restricted",
		psaWarn:                     "restricted",
		orchestrator.LabelOwner:     "owner-1",
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
	} {
		if got := ns.Labels[key]; got != want {
			t.Errorf("namespace label %s = %q, want %q", key, got, want)
		}
	}

	// A LimitRange must always exist, so a workload that requests nothing
	// still cannot run unbounded.
	lr, err := client.CoreV1().LimitRanges("ozymandis-demo").
		Get(ctx, limitRangeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get limitrange: %v", err)
	}
	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("limitrange has %d items, want 1", len(lr.Spec.Limits))
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("limitrange type = %q, want Container", item.Type)
	}
	if got := item.Default.Cpu().String(); got != "100m" {
		t.Errorf("default cpu = %q, want 100m", got)
	}
	if got := item.Max.Memory().String(); got != "4Gi" {
		t.Errorf("max memory = %q, want 4Gi", got)
	}
}

func TestEnsureNamespaceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-demo"}

	for i := range 3 {
		if err := o.EnsureNamespace(ctx, spec); err != nil {
			t.Fatalf("EnsureNamespace call %d: %v", i+1, err)
		}
	}

	list, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d namespaces after 3 applies, want 1", len(list.Items))
	}
}

// TestApplyApp is the other half of the smallest loop, and the most important
// test in the package: it asserts the hardening the engine promises is
// actually on the object that reaches the cluster.
func TestApplyApp(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	dep, err := client.AppsV1().Deployments("ozymandis-demo").
		Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", dep.Spec.Replicas)
	}

	podSpec := dep.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false")
	}
	if podSpec.SecurityContext == nil ||
		podSpec.SecurityContext.RunAsNonRoot == nil ||
		!*podSpec.SecurityContext.RunAsNonRoot {
		t.Error("pod securityContext.runAsNonRoot must be true")
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(podSpec.Containers))
	}
	c := podSpec.Containers[0]

	sc := c.SecurityContext
	switch {
	case sc == nil:
		t.Fatal("container securityContext must be set")
	case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot:
		t.Error("runAsNonRoot must be true")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("allowPrivilegeEscalation must be false")
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Error("readOnlyRootFilesystem must default to true")
	case sc.Privileged == nil || *sc.Privileged:
		t.Error("privileged must be false")
	case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 ||
		sc.Capabilities.Drop[0] != corev1.Capability("ALL"):
		t.Errorf("capabilities.drop = %v, want [ALL]", sc.Capabilities)
	case sc.SeccompProfile == nil ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Error("seccompProfile must be RuntimeDefault")
	}

	// A read-only root filesystem is only usable if /tmp is writable.
	var foundTmp bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" {
			foundTmp = true
		}
	}
	if !foundTmp {
		t.Error("expected a writable /tmp mount alongside the read-only root")
	}

	// Environment must be sorted, or every reconcile produces a different pod
	// template and restarts the workload. The injected PORT sorts in with the
	// rest rather than being appended: one invariant, no exceptions.
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	if want := []string{"APP_ENV", "LOG_LEVEL", "PORT"}; !slices.Equal(names, want) {
		t.Errorf("env = %v, want %v", names, want)
	}

	// Selector must match the pod labels, or the Deployment adopts nothing.
	for k, want := range orchestrator.SelectorLabels("web") {
		if got := dep.Spec.Selector.MatchLabels[k]; got != want {
			t.Errorf("selector[%s] = %q, want %q", k, got, want)
		}
		if got := dep.Spec.Template.Labels[k]; got != want {
			t.Errorf("pod label[%s] = %q, want %q", k, got, want)
		}
	}

	// Owner is recorded but deliberately kept out of the immutable selector.
	if _, inSelector := dep.Spec.Selector.MatchLabels[orchestrator.LabelOwner]; inSelector {
		t.Error("owner must not be part of the immutable selector")
	}
	if got := dep.Labels[orchestrator.LabelOwner]; got != "owner-1" {
		t.Errorf("owner label = %q, want owner-1", got)
	}
}

func TestApplyAppCreatesServiceOnlyWhenPortSet(t *testing.T) {
	ctx := context.Background()

	t.Run("with port", func(t *testing.T) {
		o, client := testOrchestrator(t)
		if err := o.ApplyApp(ctx, testSpec()); err != nil {
			t.Fatalf("ApplyApp: %v", err)
		}
		svc, err := client.CoreV1().Services("ozymandis-demo").
			Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get service: %v", err)
		}
		if svc.Spec.Ports[0].Port != servicePort {
			t.Errorf("service port = %d, want %d", svc.Spec.Ports[0].Port, servicePort)
		}
		if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != 8080 {
			t.Errorf("target port = %d, want 8080", got)
		}
	})

	t.Run("without port", func(t *testing.T) {
		o, client := testOrchestrator(t)
		spec := testSpec()
		spec.Port = 0
		if err := o.ApplyApp(ctx, spec); err != nil {
			t.Fatalf("ApplyApp: %v", err)
		}
		list, err := client.CoreV1().Services("ozymandis-demo").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list services: %v", err)
		}
		if len(list.Items) != 0 {
			t.Errorf("got %d services for a portless workload, want 0", len(list.Items))
		}
	})
}

// A repeated apply of an unchanged spec must not change the pod template.
// If it does, every reconcile restarts every workload.
func TestApplyAppIsStable(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := client.AppsV1().Deployments("ozymandis-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, err := client.AppsV1().Deployments("ozymandis-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	a := first.Spec.Template.Annotations[orchestrator.AnnotationRevision]
	b := second.Spec.Template.Annotations[orchestrator.AnnotationRevision]
	if a == "" {
		t.Fatal("revision annotation missing")
	}
	if a != b {
		t.Errorf("revision changed on unchanged spec: %q -> %q", a, b)
	}
}

func TestSpecHash(t *testing.T) {
	base := testSpec()

	t.Run("stable across env map ordering", func(t *testing.T) {
		other := testSpec()
		other.Env = map[string]string{"APP_ENV": "prod", "LOG_LEVEL": "info"}
		if specHash(base) != specHash(other) {
			t.Error("hash must not depend on map iteration order")
		}
	})

	t.Run("changes with image", func(t *testing.T) {
		other := testSpec()
		other.Image = "ghcr.io/example/web:v2"
		if specHash(base) == specHash(other) {
			t.Error("hash must change when the image changes")
		}
	})

	t.Run("changes with env value", func(t *testing.T) {
		other := testSpec()
		other.Env = map[string]string{"LOG_LEVEL": "debug", "APP_ENV": "prod"}
		if specHash(base) == specHash(other) {
			t.Error("hash must change when an env value changes")
		}
	})
}

// TestPortIsInjected covers the convention a buildpack-built image relies on.
// Such an image binds $PORT and binds nothing without it, so the app deploys
// green and answers 502 — this was found on a live install, not in a test, and
// only because somebody opened the URL.
func TestPortIsInjected(t *testing.T) {
	ctx := context.Background()

	// End to end for the case that failed: the value has to reach the pod
	// template, which is the only place the container ever reads it from.
	t.Run("reaches the deployment", func(t *testing.T) {
		o, client := testOrchestrator(t)
		if err := o.ApplyApp(ctx, testSpec()); err != nil {
			t.Fatalf("ApplyApp: %v", err)
		}
		dep, err := client.AppsV1().Deployments("ozymandis-demo").
			Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get deployment: %v", err)
		}

		var got string
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "PORT" {
				got = e.Value
			}
		}
		if got != "8080" {
			t.Errorf("PORT = %q, want %q (the port the container declares)", got, "8080")
		}
	})

	// A default is only a default if configuration beats it.
	t.Run("a configured PORT wins", func(t *testing.T) {
		spec := testSpec()
		spec.Env = map[string]string{"PORT": "3000"}
		if got := containerEnv(spec)["PORT"]; got != "3000" {
			t.Errorf("PORT = %q, want the configured 3000", got)
		}
	})

	// The value of a secret is not visible here, but its key is, and that is
	// all this needs to know to leave the variable alone. Emitting PORT as a
	// literal would beat the envFrom the secret arrives by.
	t.Run("a secret PORT is left alone", func(t *testing.T) {
		spec := testSpec()
		spec.Secrets = map[string]string{"PORT": "3000"}
		if _, injected := containerEnv(spec)["PORT"]; injected {
			t.Error("injected PORT over a secret of the same name")
		}
	})

	// No port means the workload takes no traffic, so there is nothing true to
	// say — and a PORT naming a port nothing routes to is worse than silence.
	t.Run("no port, no variable", func(t *testing.T) {
		spec := testSpec()
		spec.Port = 0
		if _, injected := containerEnv(spec)["PORT"]; injected {
			t.Error("injected PORT into a workload that declares no port")
		}
	})

	// The spec belongs to the caller. A mutating ApplyApp would leave PORT in
	// whatever they reuse it for next.
	t.Run("the caller's spec is untouched", func(t *testing.T) {
		spec := testSpec()
		containerEnv(spec)
		if _, leaked := spec.Env["PORT"]; leaked {
			t.Error("containerEnv wrote through to the caller's map")
		}
	})
}

func TestAppStatus(t *testing.T) {
	ctx := context.Background()
	ref := orchestrator.Ref{Owner: "owner-1", Namespace: "ozymandis-demo", Name: "web"}

	t.Run("missing workload", func(t *testing.T) {
		o, _ := testOrchestrator(t)
		_, err := o.AppStatus(ctx, ref)
		if err != orchestrator.ErrNotFound {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	cases := []struct {
		name      string
		desired   int32
		ready     int32
		wantPhase orchestrator.Phase
	}{
		{"all ready", 2, 2, orchestrator.PhaseRunning},
		{"none ready", 2, 0, orchestrator.PhasePending},
		{"partially ready", 3, 1, orchestrator.PhaseDegraded},
		{"scaled to zero", 0, 0, orchestrator.PhaseStopped},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, client := testOrchestrator(t)
			replicas := tc.desired
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ozymandis-demo"},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
				Status: appsv1.DeploymentStatus{
					ReadyReplicas:     tc.ready,
					AvailableReplicas: tc.ready,
				},
			}
			if _, err := client.AppsV1().Deployments("ozymandis-demo").
				Create(ctx, dep, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}

			got, err := o.AppStatus(ctx, ref)
			if err != nil {
				t.Fatalf("AppStatus: %v", err)
			}
			if got.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.Desired != tc.desired {
				t.Errorf("desired = %d, want %d", got.Desired, tc.desired)
			}
			if got.Ready != tc.ready {
				t.Errorf("ready = %d, want %d", got.Ready, tc.ready)
			}
		})
	}
}

func TestDeleteAppIsIdempotent(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Deleting something already gone is a no-op, not an error. Callers
	// retry, and a retry must converge rather than fail.
	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// Bad names must be rejected before they reach a cluster. A name that escapes
// its namespace is the failure mode that matters most once more than one owner
// shares a cluster.
func TestApplyAppRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	cases := []struct {
		name   string
		mutate func(*orchestrator.AppSpec)
	}{
		{"empty owner", func(s *orchestrator.AppSpec) { s.Owner = "" }},
		{"empty name", func(s *orchestrator.AppSpec) { s.Name = "" }},
		{"uppercase name", func(s *orchestrator.AppSpec) { s.Name = "Web" }},
		{"path traversal in namespace", func(s *orchestrator.AppSpec) { s.Namespace = "../kube-system" }},
		{"slash in name", func(s *orchestrator.AppSpec) { s.Name = "web/admin" }},
		{"empty image", func(s *orchestrator.AppSpec) { s.Image = "" }},
		{"negative replicas", func(s *orchestrator.AppSpec) { s.Replicas = -1 }},
		{"port out of range", func(s *orchestrator.AppSpec) { s.Port = 70000 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			tc.mutate(&spec)
			if err := o.ApplyApp(ctx, spec); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

// --- Secrets and rollouts ---

// The bug this pins, in full: secrets reach the container through envFrom,
// which names a Secret and does not carry its values. Rewriting that Secret
// therefore left the pod template byte-identical, Kubernetes saw nothing to
// roll, and the running pods kept serving the OLD values indefinitely — while
// the API reported the change as applied. A rotated password looked set and was
// not, with no error anywhere to say so.
func TestChangingASecretRollsThePods(t *testing.T) {
	base := testSpec()
	base.Secrets = map[string]string{"DATABASE_URL": "postgres://old@db/x"}

	rotated := testSpec()
	rotated.Secrets = map[string]string{"DATABASE_URL": "postgres://new@db/x"}

	if specHash(base) == specHash(rotated) {
		t.Error("a rotated secret must change the pod template, or the pods keep the old value")
	}
}

func TestSecretKeysAffectTheHash(t *testing.T) {
	base := testSpec()
	base.Secrets = map[string]string{"A": "1"}

	t.Run("adding one", func(t *testing.T) {
		other := testSpec()
		other.Secrets = map[string]string{"A": "1", "B": "2"}
		if specHash(base) == specHash(other) {
			t.Error("a new secret must roll")
		}
	})

	t.Run("removing one", func(t *testing.T) {
		other := testSpec()
		other.Secrets = map[string]string{}
		if specHash(base) == specHash(other) {
			t.Error("a deleted secret must roll — the process still has it otherwise")
		}
	})

	// Length-prefixing earns its place here. Without it these two hash alike,
	// and renaming a key between them would silently not roll.
	t.Run("a rename that preserves the concatenation", func(t *testing.T) {
		x, y := testSpec(), testSpec()
		x.Secrets = map[string]string{"AB": "C"}
		y.Secrets = map[string]string{"A": "BC"}
		if specHash(x) == specHash(y) {
			t.Error(`{"AB":"C"} and {"A":"BC"} must not hash alike`)
		}
	})

	t.Run("stable across map ordering", func(t *testing.T) {
		x, y := testSpec(), testSpec()
		x.Secrets = map[string]string{"A": "1", "B": "2", "C": "3"}
		y.Secrets = map[string]string{"C": "3", "A": "1", "B": "2"}
		if specHash(x) != specHash(y) {
			t.Error("hash must not depend on map iteration order, or every apply rolls")
		}
	})
}

// The property that must survive the fix. Hashing the values is only acceptable
// because a digest is not a value: what lands in the template must still be
// sixteen hex characters and nothing else, so `kubectl get deploy -o yaml`
// stays safe to paste into an issue.
func TestTheDeploymentNeverCarriesASecretValue(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	const password = "sup3r-s3cret-p4ssw0rd"
	spec := testSpec()
	spec.Secrets = map[string]string{"DATABASE_URL": "postgres://u:" + password + "@db/x"}

	ns := orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-demo"}
	if err := o.EnsureNamespace(ctx, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}

	dep, err := client.AppsV1().Deployments("ozymandis-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	rendered := fmt.Sprintf("%+v", dep)
	if strings.Contains(rendered, password) {
		t.Error("a secret value reached the Deployment")
	}

	rev := dep.Spec.Template.Annotations[orchestrator.AnnotationRevision]
	if len(rev) != 16 {
		t.Errorf("revision annotation = %q, want 16 hex characters", rev)
	}
	if strings.Contains(rev, password) {
		t.Error("the revision annotation contains the secret itself")
	}
}

// --- Network isolation ---

func policyFor(t *testing.T, o *Orchestrator, client kubernetes.Interface, ns orchestrator.NamespaceSpec) *networkingv1.NetworkPolicy {
	t.Helper()
	if err := o.EnsureNamespace(context.Background(), ns); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	p, err := client.NetworkingV1().NetworkPolicies(ns.Name).
		Get(context.Background(), networkPolicyName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

// Without a policy, "internal" means only that an app has no Ingress — every
// pod in the cluster can still reach it directly, at a port that checks none of
// the things the front door checks.
func TestANamespaceIsIsolatedToItsOwnerAndTheEdge(t *testing.T) {
	o, client := testOrchestrator(t)
	o.WithIngressNamespace("traefik")

	p := policyFor(t, o, client, orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-demo"})
	if p == nil {
		t.Fatal("no network policy was created")
	}

	// Ingress only. Denying egress would break every app that calls an LLM, a
	// managed database, or a payment provider — with a timeout, not an error.
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress] only", p.Spec.PolicyTypes)
	}

	// Empty pod selector: everything in the namespace, including whatever is
	// added later. A policy naming specific pods leaves the next one exposed.
	if len(p.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("podSelector = %v, want every pod", p.Spec.PodSelector.MatchLabels)
	}

	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("got %d ingress rules, want 1", len(p.Spec.Ingress))
	}
	from := p.Spec.Ingress[0].From
	if len(from) != 2 {
		t.Fatalf("got %d peers, want 2 (own owner, and the edge)", len(from))
	}

	var sawOwner, sawEdge bool
	for _, peer := range from {
		if peer.NamespaceSelector == nil {
			t.Fatal("a peer has no namespace selector, so it matches more than intended")
		}
		if peer.NamespaceSelector.MatchLabels[orchestrator.LabelOwner] == "owner-1" {
			sawOwner = true
		}
		if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "traefik" {
			sawEdge = true
		}
	}
	if !sawOwner {
		t.Error("the owner's own namespaces cannot reach each other — that breaks every wired app")
	}
	if !sawEdge {
		t.Error("the ingress controller cannot reach the app — every public app would be offline")
	}
}

// The gate. Ozymandis does not install the edge and cannot guess where it runs,
// and a policy applied without that exception denies the ingress controller
// along with everyone else: every public app offline, while the dashboard still
// reports them healthy. Off is the honest default.
func TestNoPolicyWithoutKnowingWhereTheEdgeIs(t *testing.T) {
	o, client := testOrchestrator(t)

	if p := policyFor(t, o, client, orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-demo"}); p != nil {
		t.Error("a policy was applied without an ingress namespace — public apps would be unreachable")
	}
}

// Two owners must not select each other's namespaces.
func TestOwnersDoNotSelectEachOther(t *testing.T) {
	o, client := testOrchestrator(t)
	o.WithIngressNamespace("traefik")

	a := policyFor(t, o, client, orchestrator.NamespaceSpec{Owner: "owner-1", Name: "ozymandis-a"})
	b := policyFor(t, o, client, orchestrator.NamespaceSpec{Owner: "owner-2", Name: "ozymandis-b"})
	if a == nil || b == nil {
		t.Fatal("policies missing")
	}

	ownerOf := func(p *networkingv1.NetworkPolicy) string {
		for _, peer := range p.Spec.Ingress[0].From {
			if v := peer.NamespaceSelector.MatchLabels[orchestrator.LabelOwner]; v != "" {
				return v
			}
		}
		return ""
	}
	if ownerOf(a) == ownerOf(b) {
		t.Errorf("both namespaces admit the same owner %q — that is not isolation", ownerOf(a))
	}
}

// --- Rollout completion ---

// statusFrom builds a Deployment in a given rollout state and reads it back.
func statusFrom(t *testing.T, gen, observed int64, desired, updated, total, ready, avail int32) orchestrator.AppStatus {
	t.Helper()
	o, client := testOrchestrator(t)
	ctx := context.Background()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "ozymandis-demo", Generation: gen,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: observed,
			UpdatedReplicas:    updated,
			Replicas:           total,
			ReadyReplicas:      ready,
			AvailableReplicas:  avail,
		},
	}
	if _, err := client.AppsV1().Deployments("ozymandis-demo").
		Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	st, err := o.AppStatus(ctx, orchestrator.Ref{
		Owner: "owner-1", Namespace: "ozymandis-demo", Name: "web",
	})
	if err != nil {
		t.Fatalf("AppStatus: %v", err)
	}
	return st
}

// The state that made a green deploy a lie: one new replica up, one old
// replica still serving. Ready(1) >= Desired(1) holds — which is exactly why
// waiting on Ready reported success while the previous image answered.
func TestARolloutIsNotCompleteWhileTheOldReplicaServes(t *testing.T) {
	st := statusFrom(t, 2, 2 /*desired*/, 1 /*updated*/, 1 /*total*/, 2 /*ready*/, 1 /*avail*/, 1)

	if st.Ready < st.Desired {
		t.Fatal("this test is meaningless unless Ready >= Desired here")
	}
	if st.RolloutComplete {
		t.Error("reported complete with 2 replicas for a desired of 1 — the old one is still taking traffic")
	}
	if st.Total <= st.Desired {
		t.Errorf("Total = %d, want it to show the extra replica", st.Total)
	}
}

func TestARolloutIsCompleteWhenOnlyTheNewVersionRemains(t *testing.T) {
	st := statusFrom(t, 2, 2, 1, 1, 1, 1, 1)
	if !st.RolloutComplete {
		t.Error("one updated, available replica and nothing else is a finished rollout")
	}
}

// The controller has not yet looked at the spec we applied, so every count
// below describes the PREVIOUS version and all of them can look perfect.
func TestAStaleObservationIsNotACompleteRollout(t *testing.T) {
	st := statusFrom(t /*generation*/, 3 /*observed*/, 2, 1, 1, 1, 1, 1)
	if st.RolloutComplete {
		t.Error("reported complete from counts the controller has not refreshed")
	}
}

// New pods created but not yet up. Ready would be 0 here, so this one is
// caught either way — it is pinned because availability is a separate clause
// and dropping it would be invisible in the common case.
func TestNewReplicasThatAreNotUpAreNotComplete(t *testing.T) {
	st := statusFrom(t, 2, 2 /*desired*/, 2 /*updated*/, 2 /*total*/, 2 /*ready*/, 1 /*avail*/, 1)
	if st.RolloutComplete {
		t.Error("reported complete with only one of two replicas available")
	}
}
