package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kingzion24/ozymandis/internal/app"
)

// The origin check has two threat models to serve at once, and getting either
// wrong breaks the other.
//
// A BROWSER always sends Origin, and its same-origin policy does not gate
// WebSocket upgrades the way it gates XHR. Without a check, any page a
// signed-in person visits could open a socket to this endpoint riding their
// cookie and get a shell in a production container. That is the hole.
//
// A CLI sends NO Origin — it is a browser concept — and authenticates with a
// bearer token rather than a cookie, so it cannot be the victim of a
// cross-site request in the first place: nothing ambient is attached for an
// attacker to borrow. A check strict enough to reject a missing Origin breaks
// `oz console`, and the obvious fix for that is to loosen the check back into
// the hole. So both directions are pinned here.
func TestOriginCheck(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{
			name: "a CLI sends no origin and must be allowed",
			// This is the case that breaks oz console if it is refused, and
			// whose "fix" is usually to allow everything.
			origin: "", host: "ozymandis.example", want: true,
		},
		{
			name:   "the dashboard's own page is allowed",
			origin: "https://ozymandis.example", host: "ozymandis.example", want: true,
		},
		{
			name: "another site is refused",
			// The attack: a page the person is reading opens a socket here with
			// their cookie attached.
			origin: "https://evil.example", host: "ozymandis.example", want: false,
		},
		{
			name:   "a lookalike host is refused",
			origin: "https://ozymandis.example.evil.test", host: "ozymandis.example", want: false,
		},
		{
			name:   "scheme does not matter, host does",
			origin: "http://ozymandis.example", host: "ozymandis.example", want: true,
		},
		{
			name:   "a port mismatch is a different origin",
			origin: "https://ozymandis.example:8443", host: "ozymandis.example", want: false,
		},
		{
			name:   "garbage is refused",
			origin: "://not a url", host: "ozymandis.example", want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/apps/web/exec", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}

			if got := sameOrigin(r); got != tc.want {
				t.Errorf("sameOrigin(origin=%q, host=%q) = %v, want %v",
					tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

// The console endpoint stays off the router entirely when this install cannot
// attach, rather than being mounted and failing when somebody presses it.
func TestExecEndpointIsAbsentWithoutAnExecutor(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	h, _ := testServer(t, apps, nil) // no Exec wired

	w := do(h, http.MethodGet, "/api/v1/apps/web/exec?cmd=/bin/sh", "oz_team-a-token", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
