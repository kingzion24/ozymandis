package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrNoExec means this install cannot open a console.
var ErrNoExec = errors.New(
	"app: this install cannot open a console — its orchestrator does not support exec")

// ExecTarget is the container a session will attach to, and the alternatives.
type ExecTarget struct {
	Namespace string

	// Pod is the container chosen. Empty with Note set when there is none.
	Pod string

	// Pods are every attachable pod, so a caller can offer the choice rather
	// than only announce the outcome.
	Pods []string

	// Note explains an empty Pod in words somebody can act on.
	Note string
}

// resolveExecTarget decides which container a session attaches to.
//
// The owner-scoped half of exec, and the security boundary: a pod name is
// enough to reach any tenant's container, so which pod is decided here, against
// pods this app actually has, exactly as resolveLogTarget decides it for logs.
//
// # Which pod, when there are several
//
// Not "whichever the API listed first". Logs can take that — reading output
// from an arbitrary replica is usually what somebody wants, and the page shows
// the list so they can switch. A shell cannot: running a one-off command on a
// replica chosen by list order is running it somewhere nobody decided, and the
// person will not know which. So the rule is stated rather than inherited:
//
//   - only READY pods are attachable. A crashlooping container's logs are the
//     most useful thing about it; a shell in one is a race against the restart.
//   - among those, the lowest name, sorted. Deterministic rather than
//     meaningful — there is no "best" replica — so that two consecutive
//     consoles land in the same place and a person can reason about what they
//     just did.
//   - the caller is TOLD which one, always. The choice being arbitrary is
//     exactly why it has to be visible.
//
// An explicit pod is honoured, checked against this app's list the way a log
// read checks it.
func (s *Service) resolveExecTarget(
	ctx context.Context, ownerID, appName, wantPod string,
) (ExecTarget, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return ExecTarget{}, err
	}

	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(ownerID),
	})
	if err != nil {
		return ExecTarget{}, fmt.Errorf("app: list pods for exec: %w", err)
	}

	target := ExecTarget{Namespace: a.Namespace}

	var ready []orchestrator.PodInfo
	for _, p := range pods {
		if p.App != "" && p.App != a.Name {
			continue
		}
		if execReady(p) {
			ready = append(ready, p)
			target.Pods = append(target.Pods, p.Name)
		}
	}
	slices.Sort(target.Pods)

	if len(ready) == 0 {
		target.Note = whyNoPod(pods, a)
		return target, nil
	}

	target.Pod = target.Pods[0]

	if wantPod != "" {
		if !slices.Contains(target.Pods, wantPod) {
			// The same refusal a log read gives, and for the same reason: the
			// alternative is a pod name being a way into somebody else's
			// container. Reported as not-found rather than as "not ready", so
			// the response cannot be used to learn which pods exist.
			return ExecTarget{}, ErrNotFound
		}
		target.Pod = wantPod
	}

	return target, nil
}

// execReady reports whether a pod can be attached to.
//
// Every container ready, not merely the pod running. A pod whose sidecar is up
// and whose app container is still starting is Running, and a shell opened into
// it races the thing that is about to restart it.
func execReady(p orchestrator.PodInfo) bool {
	return strings.EqualFold(p.Phase, "Running") && p.Total > 0 && p.Ready == p.Total
}

// whyNoPod turns an empty result into something a person can act on.
//
// The three states differ and the fix differs with them: scaled to zero needs a
// scale, mid-deploy needs a wait, and crashlooping needs the logs. "No pods
// available" would send all three to the same dead end — and the third is the
// one where somebody most wants a shell and least can have one.
func whyNoPod(pods []orchestrator.PodInfo, a App) string {
	if len(pods) == 0 {
		if a.Replicas == 0 {
			return "This app is scaled to zero, so there is no container to open. " +
				"Scale it up first: `oz scale 1`."
		}
		return "This app has no pods yet. If it was just deployed, give it a " +
			"moment; if it stays this way, the cluster could not schedule it."
	}

	// Pods exist but none are ready. The reason is on the pod, where Kubernetes
	// put it, and it is the whole value of this message.
	for _, p := range pods {
		if p.Reason != "" {
			return fmt.Sprintf(
				"No container is ready to open. %s is %s: %s. "+
					"Its logs will say more than a shell could.",
				p.Name, p.Reason, strings.TrimSpace(p.Message))
		}
	}
	return "No container is ready to open yet — they are still starting. " +
		"A shell opened into a container that is about to restart would go " +
		"with it, so this waits for one that is up."
}

// ExecInput is one session to open.
type ExecInput struct {
	// Pod is which container, resolved by ExecTargetFor. Required.
	Pod     string
	Command []string
	TTY     bool

	// Actor is who is opening it, for the audit row. Free text — a token's
	// user, a session's user, or the single owner of an install with no
	// accounts — because an audit row that could not be written for want of a
	// matching user is an audit row lost.
	Actor string

	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Resize         <-chan orchestrator.TerminalSize
}

// execWriteTimeout bounds the teardown write.
//
// The same shape buildLogger uses, and the reason bites harder here: a client
// vanishing is the NORMAL end of an exec session, so the teardown almost always
// runs with the caller's context already cancelled by the very disconnect that
// triggered it. Detached so the write happens at all; bounded so a write racing
// a dying database hangs the goroutine for five seconds rather than forever.
const execWriteTimeout = 5 * time.Second

// Exec opens an interactive session and records that it happened.
//
// # The audit ordering, which is the whole point
//
// The session row is written BEFORE the stream opens and closed on a DEFERRED,
// DETACHED, BOUNDED context. Both halves are load-bearing and neither is
// obvious:
//
//   - Writing the row first means a session that dies during attach — the pod
//     went away between the readiness check and the dial, the API server
//     refused the upgrade — has still been recorded. Recording at teardown
//     instead loses exactly the sessions that failed strangely, which are the
//     ones worth having.
//
//   - Closing it on a detached context means the row is closed even though the
//     context that carried the request is already cancelled. On a WebSocket the
//     client going away IS the ordinary termination, so a teardown writing
//     through the request context would fail for almost every real session and
//     succeed only for the tidy ones.
//
// A row left open is not a bug: it is a session that ended in a way this
// process never observed, and it is the signal the table exists to show.
func (s *Service) Exec(ctx context.Context, ownerID, appName string, in ExecInput) error {
	executor, ok := s.orch.(orchestrator.Executor)
	if !ok {
		return ErrNoExec
	}

	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	if in.Pod == "" {
		return errors.New("app: exec needs a pod — resolve one with ExecTargetFor")
	}
	// Re-checked here rather than trusted from the caller. ExecTargetFor is the
	// resolver, but this is the method that opens the connection, and a guard
	// that lives only in the caller is a guard some caller will skip.
	target, err := s.resolveExecTarget(ctx, ownerID, appName, in.Pod)
	if err != nil {
		return err
	}
	if target.Pod != in.Pod {
		return ErrNotFound
	}

	// BEFORE the stream. See above.
	row, startErr := s.q.StartExecSession(ctx, dbgen.StartExecSessionParams{
		OwnerID: ownerID, AppID: a.ID, Actor: in.Actor,
		Pod: in.Pod, Command: strings.Join(in.Command, " "), Tty: in.TTY,
	})
	if startErr != nil {
		// Refused rather than proceeding unrecorded. A shell this platform
		// cannot account for is worse than no shell: the entire justification
		// for offering one is that it leaves a trace.
		return fmt.Errorf("app: record the session: %w", startErr)
	}

	s.log.Info("exec session opened",
		slog.String("app", appName), slog.String("pod", in.Pod),
		slog.String("actor", in.Actor), slog.String("command", strings.Join(in.Command, " ")))

	outcome, code := ExecFailed, 0
	defer func() {
		// Detached and bounded, and deferred so every path reaches it —
		// clean exit, disconnect, cancellation, panic.
		writeCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), execWriteTimeout)
		defer cancel()

		var exitCode *int32
		if outcome == ExecExited {
			c := int32(code)
			exitCode = &c
		}
		if err := s.q.EndExecSession(writeCtx, dbgen.EndExecSessionParams{
			OwnerID: ownerID, ID: row.ID, Outcome: outcome, ExitCode: exitCode,
		}); err != nil {
			s.log.Error("close the exec session record",
				slog.String("session", row.ID.String()), slog.String("error", err.Error()))
		}
		s.log.Info("exec session closed",
			slog.String("app", appName), slog.String("pod", in.Pod),
			slog.String("outcome", outcome))
	}()

	execErr := executor.Exec(ctx, orchestrator.ExecSpec{
		Ref:     a.Ref(),
		Pod:     in.Pod,
		Command: in.Command,
		TTY:     in.TTY,
		Stdin:   in.Stdin,
		Stdout:  in.Stdout,
		Stderr:  in.Stderr,
		Resize:  in.Resize,
	})

	var exitErr *orchestrator.ExitError
	switch {
	case errors.As(execErr, &exitErr):
		outcome, code = ExecExited, exitErr.Code
	case ctx.Err() != nil:
		// The client went away. Distinct from a clean exit, because a shell
		// killed by a network blip and one that ran something and exited are
		// different events — and only the first leaves nobody knowing what was
		// done in it.
		outcome = ExecDisconnected
	case execErr != nil:
		outcome = ExecFailed
	default:
		outcome, code = ExecExited, 0
	}

	return execErr
}

// How a session ended.
const (
	ExecExited       = "exited"
	ExecDisconnected = "disconnected"
	ExecFailed       = "failed"
)

// OpenSession is a shell that has not ended.
type OpenSession struct {
	AppName   string
	Actor     string
	Pod       string
	Command   string
	StartedAt time.Time
}

// OpenSessions lists the shells that have not ended.
//
// The read side of the signal, without which the design is only theoretically
// sound: an open row that nothing can query is a record kept for nobody. Two
// situations land here and neither is distinguished, because nothing observed
// the difference — somebody working in a container right now, and a session
// killed with the process that nothing will ever close. StartedAt is what tells
// them apart, and it is the caller's to interpret.
//
// Nothing closes an open row except the session's own deferred writer. There is
// deliberately no sweep that tidies old ones: closing a row nobody watched end
// would replace the signal with a guess, which is worse than the untidiness.
func (s *Service) OpenSessions(ctx context.Context, ownerID string) ([]OpenSession, error) {
	rows, err := s.q.ListOpenExecSessions(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list open sessions: %w", err)
	}
	out := make([]OpenSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, OpenSession{
			AppName: r.AppName, Actor: r.Actor, Pod: r.Pod,
			Command: r.Command, StartedAt: r.StartedAt,
		})
	}
	return out, nil
}

// ExecTargetFor is what a caller asks before opening a session.
//
// Exported separately from Exec so a surface can report why it cannot offer a
// console — the dashboard greys the button, the CLI prints the reason — without
// opening a session to find out.
func (s *Service) ExecTargetFor(
	ctx context.Context, ownerID, appName, wantPod string,
) (ExecTarget, error) {
	return s.resolveExecTarget(ctx, ownerID, appName, wantPod)
}

// CanExec reports whether this install can open a console at all.
func (s *Service) CanExec() bool {
	_, ok := s.orch.(orchestrator.Executor)
	return ok
}
