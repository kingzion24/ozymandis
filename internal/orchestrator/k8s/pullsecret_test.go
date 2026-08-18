package k8s

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

func specWithRegistryAuth() orchestrator.AppSpec {
	s := testSpec()
	s.Image = "registry.example.com/owner-1/web:v1"
	s.RegistryAuth = []byte(`{"auths":{"registry.example.com":{"auth":"eGY6eQ=="}}}`)
	return s
}

// The reference and the Secret it names have to appear together, because the
// kubelet resolves the reference on every pod sync and reports each miss as a
// FailedToRetrieveImagePullSecret event.
func TestPullSecretIsReferencedAndCreatedTogether(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := specWithRegistryAuth()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	dep, err := client.AppsV1().Deployments(spec.Namespace).Get(
		ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	refs := dep.Spec.Template.Spec.ImagePullSecrets
	if len(refs) != 1 || refs[0].Name != orchestrator.PullSecretName {
		t.Fatalf("imagePullSecrets = %+v, want one named %q",
			refs, orchestrator.PullSecretName)
	}

	if _, err := client.CoreV1().Secrets(spec.Namespace).Get(
		ctx, orchestrator.PullSecretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the referenced pull secret was not created: %v", err)
	}
}

// A public image needs no credential, so nothing creates one — and naming a
// Secret that will never exist is what made every datastore namespace report
// FailedToRetrieveImagePullSecret on every sync, forever, for a pull that was
// succeeding the whole time.
func TestNoRegistryAuthLeavesNoDanglingReference(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	if len(spec.RegistryAuth) != 0 {
		t.Fatal("this test needs a spec with no registry auth")
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	dep, err := client.AppsV1().Deployments(spec.Namespace).Get(
		ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if refs := dep.Spec.Template.Spec.ImagePullSecrets; len(refs) != 0 {
		t.Errorf("imagePullSecrets = %+v, want none: no secret is created for "+
			"a public image, so the reference would never resolve", refs)
	}

	_, err = client.CoreV1().Secrets(spec.Namespace).Get(
		ctx, orchestrator.PullSecretName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("secret lookup err = %v, want NotFound", err)
	}
}
