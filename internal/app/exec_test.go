package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// podOrchestrator returns a fixed pod list, in a fixed (wrong) order.
//
// The order matters: it is deliberately NOT sorted, so a resolver that took
// whatever the API returned would pick a different pod than one that sorts. A
// fake that listed them alphabetically could not tell the two apart, and "which
// replica did my command run on" is the question this whole branch exists to
// answer deliberately rather than by accident.
type podOrchestrator struct {
	*orchestrator.Noop
	pods []orchestrator.PodInfo
}

func (p *podOrchestrator) Pods(
	context.Context, orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	return p.pods, nil
}

func running(name string) orchestrator.PodInfo {
	return orchestrator.PodInfo{Name: name, Phase: "Running", Ready: 1, Total: 1}
}

func execService(t *testing.T, pods ...orchestrator.PodInfo) (*Service, string) {
	t.Helper()
	s, _, pool := testService(t, Options{})
	s.orch = &podOrchestrator{Noop: orchestrator.NewNoop(), pods: pods}
	ownerID := owner(t, s, pool, "owner-exec-"+t.Name()[:min(len(t.Name()), 20)])
	return s, ownerID
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func makeApp(t *testing.T, s *Service, ownerID string, replicas int32) App {
	t.Helper()
	a, err := s.Create(context.Background(), ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: replicas,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return a
}

// The branch this file exists for.
//
// With several ready replicas the choice must be deterministic and stated, not
// "whichever the API listed first". Two consecutive consoles landing in
// different containers is how somebody runs half a fix in one place and half in
// another and cannot work out why neither took.
func TestExecPicksTheSamePodEveryTime(t *testing.T) {
	// Deliberately unsorted, the way a real API list arrives.
	s, ownerID := execService(t, running("web-c"), running("web-a"), running("web-b"))
	makeApp(t, s, ownerID, 3)

	first, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if first.Pod != "web-a" {
		t.Errorf("pod = %q, want web-a — the lowest name, not the first listed", first.Pod)
	}

	second, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if second.Pod != first.Pod {
		t.Errorf("two consecutive lookups chose %q then %q", first.Pod, second.Pod)
	}

	// And the alternatives come back, so a surface can offer the choice rather
	// than only announce the outcome.
	if len(first.Pods) != 3 {
		t.Errorf("pods = %v, want all three", first.Pods)
	}
}

// A crashlooping container's logs are the most useful thing about it. A shell
// in one is a race against the restart, so it is not offered — and the reason
// points at the logs.
func TestExecRefusesPodsThatAreNotReady(t *testing.T) {
	notReady := orchestrator.PodInfo{
		Name: "web-a", Phase: "Running", Ready: 0, Total: 1,
		Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
	}
	s, ownerID := execService(t, notReady)
	makeApp(t, s, ownerID, 1)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "" {
		t.Fatalf("attached to %q, which is not ready", target.Pod)
	}
	for _, want := range []string{"CrashLoopBackOff", "logs"} {
		if !strings.Contains(target.Note, want) {
			t.Errorf("the note does not mention %q: %q", want, target.Note)
		}
	}
}

// A pod whose sidecar is up and whose app container is still starting is
// Running. Attaching there races the thing about to restart it.
func TestExecRequiresEveryContainerReady(t *testing.T) {
	partial := orchestrator.PodInfo{Name: "web-a", Phase: "Running", Ready: 1, Total: 2}
	s, ownerID := execService(t, partial)
	makeApp(t, s, ownerID, 1)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "" {
		t.Errorf("attached to a pod with %d of %d containers ready", partial.Ready, partial.Total)
	}
}

// The three not-ready states need three different answers, because the fix
// differs: scale up, wait, or read the logs. One message for all three sends
// everybody to the same dead end.
func TestExecExplainsWhyThereIsNoPod(t *testing.T) {
	t.Run("scaled to zero says to scale up", func(t *testing.T) {
		s, ownerID := execService(t) // no pods at all
		makeApp(t, s, ownerID, 0)

		target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
		if err != nil {
			t.Fatalf("ExecTargetFor: %v", err)
		}
		if !strings.Contains(target.Note, "scaled to zero") {
			t.Errorf("note = %q, want it to name the scale", target.Note)
		}
		if !strings.Contains(target.Note, "oz scale") {
			t.Errorf("note does not say how to fix it: %q", target.Note)
		}
	})

	t.Run("no pods yet says to wait", func(t *testing.T) {
		s, ownerID := execService(t) // no pods, but replicas > 0
		makeApp(t, s, ownerID, 2)

		target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
		if err != nil {
			t.Fatalf("ExecTargetFor: %v", err)
		}
		if strings.Contains(target.Note, "scaled to zero") {
			t.Errorf("an app with replicas was reported as scaled to zero: %q", target.Note)
		}
		if !strings.Contains(target.Note, "no pods yet") {
			t.Errorf("note = %q", target.Note)
		}
	})

	t.Run("starting says so without blaming the app", func(t *testing.T) {
		starting := orchestrator.PodInfo{Name: "web-a", Phase: "Running", Ready: 0, Total: 1}
		s, ownerID := execService(t, starting)
		makeApp(t, s, ownerID, 1)

		target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
		if err != nil {
			t.Fatalf("ExecTargetFor: %v", err)
		}
		if !strings.Contains(target.Note, "still starting") {
			t.Errorf("note = %q", target.Note)
		}
	})
}

// A pod name is enough to reach any tenant's container, so a name from the
// request is honoured only if it is one of this app's — the same rule a log
// read applies, and the whole security of both paths.
func TestExecRefusesAPodThatIsNotThisApps(t *testing.T) {
	s, ownerID := execService(t, running("web-a"))
	makeApp(t, s, ownerID, 1)

	_, err := s.ExecTargetFor(context.Background(), ownerID, "web", "someone-elses-pod-0")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound — a crafted pod name must not "+
			"open a container, and must not reveal that it exists", err)
	}
}

// An explicit pod that IS this app's is honoured, so somebody debugging one
// replica can reach it.
func TestExecHonoursAnExplicitPod(t *testing.T) {
	s, ownerID := execService(t, running("web-a"), running("web-b"))
	makeApp(t, s, ownerID, 2)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "web-b")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "web-b" {
		t.Errorf("pod = %q, want web-b", target.Pod)
	}
}

// An install whose orchestrator cannot exec says so, rather than offering a
// console that fails when somebody presses it.
func TestCanExecReportsTheOrchestratorsCapability(t *testing.T) {
	s, _ := execService(t)
	if s.CanExec() {
		t.Error("an orchestrator with no Exec reported that it can open a console")
	}
}

// created returns a ready pod with an age, so a rollout can be described.
func created(name string, age time.Duration) orchestrator.PodInfo {
	p := running(name)
	p.CreatedAt = time.Now().Add(-age)
	return p
}

// The gap this closes.
//
// A terminating pod keeps phase Running with every container ready for the
// whole of its grace period, so readiness alone cannot tell it from the pod
// replacing it. Sorting by name then hands out a shell in whichever sorts
// first — and mid-rollout that is a coin toss between the old image and the
// new one. Somebody opening a console to check a deploy gets told the previous
// release is live.
func TestExecSkipsAPodThatIsShuttingDown(t *testing.T) {
	outgoing := created("web-a", 2*time.Hour) // sorts first, and is going away
	outgoing.Terminating = true
	incoming := created("web-b", time.Minute)

	s, ownerID := execService(t, outgoing, incoming)
	makeApp(t, s, ownerID, 1)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "web-b" {
		t.Errorf("pod = %q, want web-b — web-a is terminating however ready it looks", target.Pod)
	}
	// And it is not offered as an alternative either: a pod nobody may attach
	// to has no business in the list somebody picks from.
	if slices.Contains(target.Pods, "web-a") {
		t.Errorf("pods = %v, still offers the terminating pod", target.Pods)
	}
}

// Naming it explicitly is refused for the same reason choosing it is.
func TestExecRefusesAnExplicitPodThatIsShuttingDown(t *testing.T) {
	outgoing := created("web-a", 2*time.Hour)
	outgoing.Terminating = true
	s, ownerID := execService(t, outgoing, created("web-b", time.Minute))
	makeApp(t, s, ownerID, 1)

	if _, err := s.ExecTargetFor(context.Background(), ownerID, "web", "web-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Replicas of one release are interchangeable; mid-rollout they are not
// replicas of the same thing. The new one is what somebody asking now means,
// even when the old one sorts first and is not yet terminating.
func TestExecPrefersTheNewestPod(t *testing.T) {
	s, ownerID := execService(t,
		created("web-a", 3*time.Hour), // older release, still up
		created("web-c", time.Minute), // the one just rolled out
		created("web-b", 2*time.Hour),
	)
	makeApp(t, s, ownerID, 3)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "web-c" {
		t.Errorf("pod = %q, want web-c — the newest, not the lowest name", target.Pod)
	}
}

// Same age is the settled case, and there the old rule still holds: pick by
// name so two consecutive consoles land together.
func TestExecStillBreaksTiesByName(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	a, b, c := running("web-c"), running("web-a"), running("web-b")
	a.CreatedAt, b.CreatedAt, c.CreatedAt = at, at, at

	s, ownerID := execService(t, a, b, c)
	makeApp(t, s, ownerID, 3)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "web-a" {
		t.Errorf("pod = %q, want web-a — same age, so the lowest name decides", target.Pod)
	}
}

// "Still starting" would send somebody hunting a fault that is not there.
func TestExecSaysWhenEveryPodIsShuttingDown(t *testing.T) {
	a, b := created("web-a", time.Hour), created("web-b", time.Hour)
	a.Terminating, b.Terminating = true, true

	s, ownerID := execService(t, a, b)
	makeApp(t, s, ownerID, 2)

	target, err := s.ExecTargetFor(context.Background(), ownerID, "web", "")
	if err != nil {
		t.Fatalf("ExecTargetFor: %v", err)
	}
	if target.Pod != "" {
		t.Fatalf("attached to %q, which is shutting down", target.Pod)
	}
	if !strings.Contains(target.Note, "shutting down") {
		t.Errorf("the note does not say they are shutting down: %q", target.Note)
	}
}
