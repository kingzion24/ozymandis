package k8s

import (
	"context"
	"slices"
	"testing"
)

// The whole point of the field: one image, two workloads. A queue consumer and
// the web process that shares its code deploy separately and scale separately,
// which on a platform without this needs two images that cannot drift apart.
func TestACommandReplacesTheImageEntrypoint(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Name = "consumer"
	spec.Port = 0
	spec.Command = []string{"python", "log_consumer.py"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	c := containerOf(t, client, spec.Namespace, "consumer")
	want := []string{"python", "log_consumer.py"}
	if !slices.Equal(c.Command, want) {
		t.Errorf("command = %q, want %q", c.Command, want)
	}

	// Args must stay empty. Kubernetes drops the image's CMD only when args is
	// unset; writing anything here would append it to the command instead.
	if len(c.Args) != 0 {
		t.Errorf("args = %q, want none — a set args would append the image CMD", c.Args)
	}
}

// The default has to stay "run what the image says", because that is every app
// deployed from a purpose-built image and it must not acquire an empty command.
func TestNoCommandLeavesTheImageEntrypointAlone(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	c := containerOf(t, client, spec.Namespace, spec.Name)
	if len(c.Command) != 0 {
		t.Errorf("command = %q, want none", c.Command)
	}
}

// A command is part of what the workload is, so changing it has to produce a
// different revision — otherwise the apply is a no-op and the old process keeps
// running under a spec that says otherwise.
func TestChangingTheCommandChangesTheRevision(t *testing.T) {
	base := testSpec()

	withCommand := base
	withCommand.Command = []string{"python", "log_consumer.py"}
	if specHash(base) == specHash(withCommand) {
		t.Error("hash must change when a command is added")
	}

	other := base
	other.Command = []string{"python", "other_consumer.py"}
	if specHash(withCommand) == specHash(other) {
		t.Error("hash must change when the command changes")
	}
}

// Validation belongs where somebody can read the reason. Kubernetes accepts an
// empty argv[0] and the kubelet then reports an executable named "".
func TestACommandStartingEmptyFailsValidation(t *testing.T) {
	spec := testSpec()
	spec.Command = []string{"", "server"}
	if err := spec.Validate(); err == nil {
		t.Fatal("Validate = nil, want a refusal")
	}
}

func TestAWorkloadWithNoPortStillAcceptsACommand(t *testing.T) {
	spec := testSpec()
	spec.Port = 0
	spec.Hosts = nil
	spec.Command = []string{"python", "log_consumer.py"}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
