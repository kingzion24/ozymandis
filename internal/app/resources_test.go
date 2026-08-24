package app

import (
	"context"
	"testing"
)

// SetResources always replaces the whole set, the same convention SetService
// uses for port and internal together. This is what lets it start from an app
// with nothing set and land on exactly the four values given.
func TestSetResourcesWritesAllFourFields(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-resources-set")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	a, err := s.SetResources(ctx, ownerID, "web", "250m", "500m", "256Mi", "512Mi")
	if err != nil {
		t.Fatalf("SetResources: %v", err)
	}
	if a.CPURequest != "250m" || a.CPULimit != "500m" ||
		a.MemoryRequest != "256Mi" || a.MemoryLimit != "512Mi" {
		t.Fatalf("SetResources returned %+v, want 250m/500m/256Mi/512Mi", a)
	}

	// Persisted, not only returned — a second read must agree with the write.
	got, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CPURequest != "250m" || got.CPULimit != "500m" ||
		got.MemoryRequest != "256Mi" || got.MemoryLimit != "512Mi" {
		t.Fatalf("stored resources = %+v, want 250m/500m/256Mi/512Mi", got)
	}
}

// Empty clears a field back to the namespace default. Without this, there
// would be no way to undo a mistaken oz resources set short of picking a
// number that happens to match the default.
func TestSetResourcesEmptyClearsToDefault(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-resources-clear")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetResources(ctx, ownerID, "web", "250m", "500m", "256Mi", "512Mi"); err != nil {
		t.Fatalf("set: %v", err)
	}

	a, err := s.SetResources(ctx, ownerID, "web", "", "", "", "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if a.CPURequest != "" || a.CPULimit != "" || a.MemoryRequest != "" || a.MemoryLimit != "" {
		t.Fatalf("SetResources with empty values left %+v, want all cleared", a)
	}
}

// A value above the install's ceiling is refused here, naming what was typed
// and what the ceiling is — not left to fail at the cluster as an admission
// rejection with nothing about which app or which field caused it.
func TestSetResourcesRejectsAboveTheCeiling(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-resources-ceiling")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.SetResources(ctx, ownerID, "web", "", "4", "", ""); err == nil {
		t.Fatal("SetResources accepted a cpu limit above the namespace ceiling")
	}

	// Refused, and nothing was written — a rejected call must not leave the
	// app half-changed.
	got, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CPULimit != "" {
		t.Fatalf("cpu limit = %q after a rejected call, want unchanged", got.CPULimit)
	}
}

// Garbage that cannot even be parsed is refused the same way, before it
// reaches the ceiling check or the database.
func TestSetResourcesRejectsGarbage(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-resources-garbage")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.SetResources(ctx, ownerID, "web", "not-a-quantity", "", "", ""); err == nil {
		t.Fatal("SetResources accepted an unparsable cpu request")
	}
}

func TestSetResourcesOnMissingAppFails(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-resources-missing")

	if _, err := s.SetResources(ctx, ownerID, "does-not-exist", "250m", "", "", ""); err == nil {
		t.Fatal("SetResources succeeded on an app that does not exist")
	}
}
