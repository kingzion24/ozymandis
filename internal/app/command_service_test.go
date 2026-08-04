package app

import (
	"context"
	"slices"
	"testing"
)

// The shape this field exists for: one image, deployed twice, running a
// different process each time.
func TestACommandReachesTheCluster(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-command")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "consumer", Image: "ghcr.io/example/mcp:v1",
		Replicas: 1, Command: "python log_consumer.py",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{"python", "log_consumer.py"}
	if got := orch.lastAppSpec().Command; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestNoCommandReachesTheClusterAsNone(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-nocommand")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := orch.lastAppSpec().Command; len(got) != 0 {
		t.Errorf("command = %q, want none", got)
	}
}

// Stored as typed so the form shows back what was written, rather than a
// re-quoted rendering of what it was understood to mean.
func TestACommandRoundTripsAsItWasTyped(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-cmd-roundtrip")

	const typed = `uvicorn server:app --log-config '{"version": 1}'`
	if _, err := s.Create(ctx, id, CreateInput{
		Name: "api", Image: "ghcr.io/example/mcp:v1", Replicas: 1,
		Port: 8000, Command: typed,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, id, "api")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Command != typed {
		t.Errorf("command = %q, want %q", got.Command, typed)
	}
}

// Refused at the form, where the person who typed the quote is still looking at
// it — not several steps later at the apply.
func TestAnUnparseableCommandIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-cmd-bad")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "broken", Image: "nginx:alpine", Replicas: 1,
		Command: `python "unclosed`,
	}); err == nil {
		t.Fatal("Create = nil error, want a refusal")
	}

	if _, err := s.Get(ctx, id, "broken"); err == nil {
		t.Error("the app was written despite the refusal")
	}
}

func TestSetCommandAppliesImmediately(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-setcommand")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "worker", Image: "ghcr.io/example/mcp:v1", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.SetCommand(ctx, id, "worker", "python log_consumer.py"); err != nil {
		t.Fatalf("SetCommand: %v", err)
	}

	want := []string{"python", "log_consumer.py"}
	if got := orch.lastAppSpec().Command; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q — the change must reach the cluster now, "+
			"not at the next deploy", got, want)
	}

	// And back to the image's own.
	if err := s.SetCommand(ctx, id, "worker", "  "); err != nil {
		t.Fatalf("SetCommand to empty: %v", err)
	}
	if got := orch.lastAppSpec().Command; len(got) != 0 {
		t.Errorf("command = %q, want none after clearing", got)
	}
}

func TestSetCommandRefusesAnUnparseableLine(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-setcommand-bad")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "worker", Image: "ghcr.io/example/mcp:v1", Replicas: 1,
		Command: "python log_consumer.py",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.SetCommand(ctx, id, "worker", `python 'unclosed`); err == nil {
		t.Fatal("SetCommand = nil error, want a refusal")
	}

	// The refusal must leave the working command in place rather than half-
	// applying: a worker running the wrong process is worse than one unchanged.
	got, err := s.Get(ctx, id, "worker")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Command != "python log_consumer.py" {
		t.Errorf("command = %q, want the original to survive the refusal", got.Command)
	}
}
