package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// capture runs f with Out and Err pointed at pipes and returns what each got.
//
// Real *os.File pipes rather than buffers, because Env carries *os.File and the
// point of these tests is which STREAM a line lands on. A buffer would prove
// the text exists and nothing about where it went — and "the diff went to
// stdout" is precisely the bug that would corrupt `oz config > ozymandis.toml`.
func capture(t *testing.T, f func(env *Env)) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	done := make(chan struct{})
	var outBuf, errBuf strings.Builder
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			if n > 0 {
				outBuf.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	doneErr := make(chan struct{})
	go func() {
		defer close(doneErr)
		buf := make([]byte, 4096)
		for {
			n, err := errR.Read(buf)
			if n > 0 {
				errBuf.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	f(&Env{Out: outW, Err: errW, ContextName: "test"})

	outW.Close()
	errW.Close()
	<-done
	<-doneErr
	return outBuf.String(), errBuf.String()
}

// The requirement inherited from stage 1c.
//
// The server is careful to report that it will NOT apply something, with a
// reason. A CLI that swallowed that would waste the care entirely: a
// Skipped:true field in a JSON response nobody reads is the preview-lies
// problem moved up one level. Somebody who edits [build] image and runs
// `oz deploy` has to be told the converge did not apply it — on every run, not
// behind a flag.
func TestDeployReportsSkippedChangesOnStderr(t *testing.T) {
	result := ConfigResult{Changes: []Change{
		{Field: "health.path", From: "", To: "/healthz", Axis: "declarative"},
		{
			Field: "build.image", From: "nginx:1", To: "nginx:2", Axis: "declarative",
			Skipped: true,
			Reason: "recorded, not applied here: build.image changes what runs, so " +
				"it is applied by a deploy rather than by a config converge.",
		},
		{
			Field: "scale.replicas", From: "10", To: "2", Axis: "operational",
			Skipped: true, Reason: "replicas is operational: pass --scale to apply it.",
		},
	}}

	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	for _, want := range []string{
		"build.image", "nginx:1", "nginx:2",
		"applied by a deploy",
		"scale.replicas", "--scale",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q — a skip nobody reads is not a "+
				"skip that was reported.\n%s", want, stderr)
		}
	}

	// And the applied one is there too, distinguishable from the skips.
	if !strings.Contains(stderr, "health.path") {
		t.Error("the applied change was not reported")
	}
	if !strings.Contains(stderr, "Not applied by this converge") {
		t.Errorf("skipped changes are not visibly separated from applied ones:\n%s", stderr)
	}
}

// A reason attached to an APPLIED change is a caveat, and must be printed too.
//
// The server can attach one to a change it is going ahead with — a release
// command on an app whose volumes the release will not see. A CLI that printed
// reasons only for skips would swallow it, which is the same failure as
// swallowing a skip: the server took the trouble to say something and nobody
// hears it.
func TestReasonsOnAppliedChangesArePrintedToo(t *testing.T) {
	result := ConfigResult{Changes: []Change{{
		Field: "deploy.release_command", From: "", To: "./migrate",
		Axis: "declarative",
		Reason: "note: this app has storage, and a release runs beside the app " +
			"rather than in it — its volumes are not mounted.",
	}}}

	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	if !strings.Contains(stderr, "volumes are not mounted") {
		t.Errorf("a caveat on an applied change was swallowed:\n%s", stderr)
	}
	// And it is still reported as something that IS happening.
	if strings.Contains(stderr, "Not applied by this converge") {
		t.Errorf("an applied change was filed under skipped:\n%s", stderr)
	}
}

// The other half of the caveat channel: an applied change with NO reason stays
// quiet.
//
// Printing reasons for applied changes makes stderr a general advisory channel,
// which is useful — and the cost of a general channel is that it must be silent
// by default. Without this, the next person attaching a Reason for some
// unrelated purpose gets surprise output on every deploy, and the signal that
// made the channel worth having stops being a signal.
func TestAppliedChangesWithoutAReasonStayQuiet(t *testing.T) {
	result := ConfigResult{Changes: []Change{
		{Field: "health.path", From: "", To: "/healthz", Axis: "declarative"},
		{Field: "env.LOG_LEVEL", From: "info", To: "debug", Axis: "declarative"},
	}}

	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	// Two changes, so two lines plus the heading. Anything more means something
	// is being said that nobody asked for.
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 3 {
		t.Errorf("a converge with no caveats printed %d lines, want 3:\n%s",
			len(lines), stderr)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "      ") {
			t.Errorf("an indented caveat line appeared with no Reason set: %q", l)
		}
	}
}

// Every human-readable line goes to stderr, so that stdout carries only data.
// `oz config > ozymandis.toml` must not end up with a change log in the file.
func TestDeployWritesNothingToStdout(t *testing.T) {
	result := ConfigResult{
		Changes: []Change{
			{Field: "health.path", To: "/healthz"},
			{Field: "build.image", To: "x", Skipped: true, Reason: "why"},
		},
		UntrackedDomains: []string{"kept.example.com"},
	}

	stdout, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	if stdout != "" {
		t.Errorf("reportChanges wrote to stdout:\n%q", stdout)
	}
	if stderr == "" {
		t.Error("nothing was reported at all")
	}
}

// Untracked domains are reported on every run, because the file is not a
// faithful description of the app's routing and somebody reading only the file
// would never learn that.
func TestUntrackedDomainsAreReported(t *testing.T) {
	result := ConfigResult{UntrackedDomains: []string{"a.example.com", "b.example.com"}}

	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	for _, host := range result.UntrackedDomains {
		if !strings.Contains(stderr, host) {
			t.Errorf("stderr does not mention %s:\n%s", host, stderr)
		}
	}
	if !strings.Contains(stderr, "oz domains remove") {
		t.Errorf("the report does not say how to remove one:\n%s", stderr)
	}
}

// A dry run says so in words. A preview that printed a diff and exited silently
// reads exactly like a deploy that happened.
func TestDryRunUsesConditionalWording(t *testing.T) {
	result := ConfigResult{
		DryRun:  true,
		Changes: []Change{{Field: "health.path", To: "/healthz"}},
	}

	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	if !strings.Contains(stderr, "Would change") {
		t.Errorf("a dry run reported changes in the past tense:\n%s", stderr)
	}
	if strings.Contains(stderr, "Changed:") {
		t.Errorf("a dry run claimed it changed something:\n%s", stderr)
	}
}

func TestNoChangesSaysSo(t *testing.T) {
	_, stderr := capture(t, func(env *Env) { reportChanges(env, ConfigResult{}) })
	if !strings.Contains(stderr, "already matches") {
		t.Errorf("a no-op converge said nothing:\n%s", stderr)
	}
}

// An empty value renders as something visible. "env.FOO:  → " is unreadable,
// and worse, hides which side was empty.
func TestEmptyValuesAreLegible(t *testing.T) {
	result := ConfigResult{Changes: []Change{{Field: "env.GONE", From: "x", To: ""}}}
	_, stderr := capture(t, func(env *Env) { reportChanges(env, result) })

	if !strings.Contains(stderr, "(unset)") {
		t.Errorf("an empty value rendered as nothing at all:\n%s", stderr)
	}
}

// --watch exists so a pipeline can trust the exit code. A superseded deploy is
// finished and not failed, and reporting it as deployed told CI a commit was
// live when nothing of it had been applied.
func TestOnlyAnActiveDeploymentCountsAsDeployed(t *testing.T) {
	for status, wantErr := range map[string]bool{
		"active":     false,
		"failed":     true,
		"superseded": true,
		"cancelled":  true,
	} {
		err := deployOutcome("web", Deployment{Status: status, Finished: true})
		if (err != nil) != wantErr {
			t.Errorf("status %q: err = %v, want an error: %v", status, err, wantErr)
		}
	}
	if err := deployOutcome("web", Deployment{Status: "superseded"}); !errors.Is(err, errSuperseded) {
		t.Errorf("a superseded deploy is not distinguishable from a failed one: %v", err)
	}
}

// statusServer answers /status with each body in turn, repeating the last.
func statusServer(t *testing.T, bodies ...string) *Env {
	t.Helper()
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := bodies[min(calls, len(bodies)-1)]
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { null.Close() })
	return &Env{
		Client: NewClient(Context{Endpoint: ts.URL, Token: "t"}),
		Out:    null, Err: null,
	}
}

// "active" is the cluster accepting a version, not that version starting. The
// watch has to see the rollout through, or a crash-looping image is reported as
// deployed while the old pods keep serving.
func TestWatchWaitsForTheRolloutToComplete(t *testing.T) {
	env := statusServer(t,
		`{"desired":2,"ready":1,"rollout_complete":false}`,
		`{"desired":2,"ready":2,"rollout_complete":true}`,
	)
	if err := waitForRollout(context.Background(), env, "web", time.Millisecond, time.Second); err != nil {
		t.Fatalf("a rollout that completed was reported as failed: %v", err)
	}
}

func TestWatchFailsWhenTheNewVersionNeverTakesOver(t *testing.T) {
	env := statusServer(t,
		`{"desired":1,"ready":0,"message":"ImagePullBackOff","rollout_complete":false}`)

	err := waitForRollout(context.Background(), env, "web", time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("a rollout that never completed was reported as deployed")
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("the error does not say why: %v", err)
	}
}
