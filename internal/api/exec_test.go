package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
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

// scriptedExec is a command that reads its input to the end, then answers.
//
// Both halves matter: draining stdin is what a real command does, and the
// output and exit code are produced only AFTER that, which is precisely the
// window the bug below closed.
type scriptedExec struct{}

func (scriptedExec) CanExec() bool { return true }

func (scriptedExec) ExecTargetFor(
	_ context.Context, _, _, _ string,
) (app.ExecTarget, error) {
	return app.ExecTarget{Namespace: "ozymandis-aaaa", Pod: "web-1"}, nil
}

func (scriptedExec) Exec(ctx context.Context, _, _ string, in app.ExecInput) error {
	if _, err := io.Copy(io.Discard, in.Stdin); err != nil {
		return err
	}
	// Cancelled while reading stdin means the session was torn down by the end
	// of input rather than by the client leaving. Reported as itself, so the
	// test names that rather than a missing frame.
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := in.Stdout.Write([]byte("OUT")); err != nil {
		return err
	}
	return &orchestrator.ExitError{Code: 7}
}

// The end of stdin must end stdin, and nothing else.
//
// A non-interactive `oz exec -- ls` has stdin at EOF before the command has
// produced a byte. Saying so with a close frame ended the whole session: the
// server read the close as the client leaving, cancelled the context, and
// killed the command mid-flight. The client then saw a normal closure and
// reported success — so every scripted exec printed nothing and exited 0,
// whatever the command actually did.
func TestEndOfStdinDoesNotEndTheSession(t *testing.T) {
	apps := newFakeApps()
	apps.add("team-a", app.App{Name: "web"})
	ident := &tokenIdentity{
		tokens: map[string]string{"oz_team-a-token": "team-a"},
		roles:  map[string]account.Role{"oz_team-a-token": account.RoleAdmin},
	}
	srv, err := New(Options{
		Identity: ident, Apps: apps, Roles: ident,
		Exec: scriptedExec{}, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/apps/web/exec?cmd=ls"
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{
		"Authorization": []string{"Bearer oz_team-a-token"},
	})
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// What the CLI sends when stdin runs out. The bug was sending a close here.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"stdin":"eof"}`)); err != nil {
		t.Fatalf("send end-of-stdin: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var out []byte
	var final exit
	for final == (exit{}) {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("the session ended before the command answered "+
				"(output so far %q): %v", out, err)
		}
		switch kind {
		case websocket.BinaryMessage:
			out = append(out, data...)
		case websocket.TextMessage:
			if err := json.Unmarshal(data, &final); err != nil {
				t.Fatalf("final frame %q: %v", data, err)
			}
		}
	}

	if string(out) != "OUT" {
		t.Errorf("output = %q, want %q — the command's stdout was lost", out, "OUT")
	}
	if final.Error != "" {
		t.Errorf("error = %q, want none", final.Error)
	}
	if final.Exit != 7 {
		t.Errorf("exit = %d, want 7 — a script cannot see what the command saw",
			final.Exit)
	}
}
