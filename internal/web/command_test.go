package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestCommandSetReachesTheService(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/command",
		url.Values{"command": {"python log_consumer.py"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := apps.commands["web"]; got != "python log_consumer.py" {
		t.Errorf("command = %q, want the form value", got)
	}
}

func TestCommandCanBeCleared(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	if rec := post(t, h, "/apps/web/command", url.Values{"command": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got, ok := apps.commands["web"]; !ok || got != "" {
		t.Errorf("command = %q (set=%v), want an empty string to reach the service", got, ok)
	}
}

// The refusal has to come back as a page with the reason on it, not a 303 that
// looks exactly like success.
func TestAnUnparseableCommandIsRefusedWithTheReason(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/command", url.Values{"command": {`python "unclosed`}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "unclosed double quote") {
		t.Error("the page does not say why it refused")
	}
	if _, ok := apps.commands["web"]; ok {
		t.Error("the command was stored despite the refusal")
	}
}

// Mutations must be POST: a GET that changes state can be triggered by a
// prefetch or a crawler.
func TestCommandRejectsGET(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	if rec := get(t, h, "/apps/web/command"); rec.Code == http.StatusSeeOther {
		t.Errorf("GET status = %d, want a refusal", rec.Code)
	}
	if len(apps.commands) != 0 {
		t.Error("a GET changed state")
	}
}

// The settings tab has to show the command back, or there is no way to see what
// an app is running without reading the cluster.
func TestTheSettingsTabShowsTheCommand(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Command = "python log_consumer.py"
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web/settings").Body.String()
	if !strings.Contains(body, "python log_consumer.py") {
		t.Error("the settings tab does not show the command")
	}
}
