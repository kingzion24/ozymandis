package main

import "testing"

func TestMergeResourceFlagsChangesOnlyWhatWasPassed(t *testing.T) {
	current := App{
		Name: "web", CPURequest: "100m", CPULimit: "200m",
		MemoryRequest: "128Mi", MemoryLimit: "256Mi",
	}

	got := mergeResourceFlags(current, map[string]string{"cpu-limit": "500m"})

	want := current
	want.CPULimit = "500m"
	if got != want {
		t.Errorf("mergeResourceFlags(current, {cpu-limit: 500m}) = %+v, want %+v", got, want)
	}
}

// A flag passed as an empty string is a real instruction — clear this field —
// and must be told apart from a flag that was never mentioned at all. The
// caller (resourcesSet) is what makes that distinction, via fs.Visit; this
// only has to trust the map it is given.
func TestMergeResourceFlagsAppliesAnExplicitEmptyValue(t *testing.T) {
	current := App{Name: "web", CPULimit: "500m", MemoryLimit: "512Mi"}

	got := mergeResourceFlags(current, map[string]string{"cpu-limit": ""})

	if got.CPULimit != "" {
		t.Errorf("cpu limit = %q, want cleared", got.CPULimit)
	}
	if got.MemoryLimit != "512Mi" {
		t.Errorf("memory limit = %q, want unchanged", got.MemoryLimit)
	}
}

func TestMergeResourceFlagsWithNothingChangedReturnsCurrent(t *testing.T) {
	current := App{Name: "web", CPURequest: "100m"}
	if got := mergeResourceFlags(current, map[string]string{}); got != current {
		t.Errorf("mergeResourceFlags(current, {}) = %+v, want %+v unchanged", got, current)
	}
}

func TestMergeResourceFlagsCanChangeAllFour(t *testing.T) {
	current := App{Name: "web"}
	got := mergeResourceFlags(current, map[string]string{
		"cpu-request": "250m", "cpu-limit": "500m",
		"memory-request": "256Mi", "memory-limit": "512Mi",
	})
	want := App{
		Name: "web", CPURequest: "250m", CPULimit: "500m",
		MemoryRequest: "256Mi", MemoryLimit: "512Mi",
	}
	if got != want {
		t.Errorf("mergeResourceFlags = %+v, want %+v", got, want)
	}
}

func TestRequestLimitOf(t *testing.T) {
	for _, tc := range []struct {
		request, limit, want string
	}{
		{"", "", "default"},
		{"250m", "500m", "250m/500m"},
		{"250m", "", "250m/—"},
		{"", "500m", "—/500m"},
	} {
		if got := requestLimitOf(tc.request, tc.limit); got != tc.want {
			t.Errorf("requestLimitOf(%q, %q) = %q, want %q", tc.request, tc.limit, got, tc.want)
		}
	}
}
