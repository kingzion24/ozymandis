package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/app"
)

func TestResourcesSetReachesTheService(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/resources", url.Values{
		"cpu_request": {"250m"}, "cpu_limit": {"500m"},
		"memory_request": {"256Mi"}, "memory_limit": {"512Mi"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := apps.resources["web"]; got != "250m/500m/256Mi/512Mi" {
		t.Errorf("resources = %q, want 250m/500m/256Mi/512Mi", got)
	}
}

// Every field is always in the POST body, blank when its input was left
// empty — this is what a browser submitting the form actually sends, and it
// is what clears a field back to the namespace default.
func TestResourcesCanBeCleared(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/resources", url.Values{
		"cpu_request": {""}, "cpu_limit": {""},
		"memory_request": {""}, "memory_limit": {""},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got, ok := apps.resources["web"]; !ok || got != "///" {
		t.Errorf("resources = %q (set=%v), want all empty to reach the service", got, ok)
	}
}

// A value the service refuses — over the namespace ceiling, or unparsable —
// has to come back as a page with the reason on it, not a 303 that looks like
// success.
func TestAnInvalidResourceValueIsRefusedWithTheReason(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	apps.err = app.ErrInvalidResources
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/resources", url.Values{"cpu_limit": {"4"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

// Mutations must be POST: a GET that changes state can be triggered by a
// prefetch or a crawler.
func TestResourcesRejectsGET(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	if rec := get(t, h, "/apps/web/resources"); rec.Code == http.StatusSeeOther {
		t.Errorf("GET status = %d, want a refusal", rec.Code)
	}
	if len(apps.resources) != 0 {
		t.Error("a GET changed state")
	}
}

// The settings tab has to show the current values back, pre-filled into the
// form, or changing one field means retyping every other one from memory.
func TestTheSettingsTabShowsCurrentResources(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.CPURequest, a.CPULimit = "250m", "500m"
	a.MemoryRequest, a.MemoryLimit = "256Mi", "512Mi"
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web/settings").Body.String()
	for _, want := range []string{"250m", "500m", "256Mi", "512Mi"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings tab does not show %q", want)
		}
	}
}
