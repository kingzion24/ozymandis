package k8s

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// A build runs a stranger's Dockerfile on the machine the apps and the control
// plane's database share. It needs an end the cluster enforces and a ceiling on
// what it can take.
func TestABuildJobIsBounded(t *testing.T) {
	job := buildJob("build-x", buildReq(), "registry-secret", "")

	deadline := job.Spec.ActiveDeadlineSeconds
	if deadline == nil {
		t.Fatal("no ActiveDeadlineSeconds: a build whose process restarted would run for ever")
	}
	if got, min := time.Duration(*deadline)*time.Second, buildTimeout; got <= min {
		t.Errorf("deadline %s is not past the process's own %s cap, which can say why", got, min)
	}

	pod := job.Spec.Template.Spec
	steps := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
	if len(steps) < 3 {
		t.Fatalf("found %d build steps, want the clone, the Dockerfile build and the buildpack", len(steps))
	}
	for _, c := range steps {
		if c.Resources.Limits.Memory().IsZero() {
			t.Errorf("step %q has no memory limit", c.Name)
		}
		if c.Resources.Requests.Memory().IsZero() {
			t.Errorf("step %q has no memory request", c.Name)
		}
		// CPU is deliberately unlimited: it is shared by request under
		// contention, and a limit would only slow builds on an idle node.
		if !c.Resources.Limits.Cpu().IsZero() {
			t.Errorf("step %q has a CPU limit", c.Name)
		}
	}
}

// A task left to the namespace default gets 128Mi — sized for a small web
// process, not for a migration or a pg_dump piped into restic.
func TestATaskStatesItsOwnResources(t *testing.T) {
	job := taskJob(orchestrator.TaskSpec{
		Ref:     orchestrator.Ref{Owner: "team-a", Namespace: "ozymandis-a", Name: "web-release"},
		Image:   "alpine:3",
		Command: []string{"true"},
	})

	c := job.Spec.Template.Spec.Containers[0]
	limit := c.Resources.Limits.Memory()
	if limit.IsZero() {
		t.Fatal("the task has no memory limit of its own, so it runs at the namespace default")
	}
	if limit.Cmp(*orchestrator128Mi()) <= 0 {
		t.Errorf("memory limit %s is no more than the 128Mi default it exists to escape", limit)
	}
	if c.Resources.Requests.Memory().Cmp(*limit) >= 0 {
		t.Error("the request is as large as the limit, so a task could fail to schedule on a busy node")
	}
}

func orchestrator128Mi() *resource.Quantity {
	q := resource.MustParse(orchestrator.DefaultLimits.DefaultMemory)
	return &q
}
