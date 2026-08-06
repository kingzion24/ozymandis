package app

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// execOrchestrator can attach, and can be made to fail in specific ways.
type execOrchestrator struct {
	*orchestrator.Noop
	pods []orchestrator.PodInfo

	// onExec runs instead of a real session. It receives the context the
	// service passed, which is what lets a test simulate a client vanishing
	// mid-session by cancelling from outside and returning.
	onExec func(ctx context.Context, spec orchestrator.ExecSpec) error
}

func (e *execOrchestrator) Pods(
	context.Context, orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	return e.pods, nil
}

func (e *execOrchestrator) Exec(ctx context.Context, spec orchestrator.ExecSpec) error {
	if e.onExec != nil {
		return e.onExec(ctx, spec)
	}
	return nil
}

var _ orchestrator.Executor = (*execOrchestrator)(nil)

// execAuditService wires a service whose orchestrator can attach.
func execAuditService(t *testing.T, name string) (*Service, *execOrchestrator, string) {
	t.Helper()
	s, _, pool := testService(t, Options{})
	orch := &execOrchestrator{
		Noop: orchestrator.NewNoop(),
		pods: []orchestrator.PodInfo{running("web-a")},
	}
	s.orch = orch
	ownerID := owner(t, s, pool, name)

	if _, err := s.Create(context.Background(), ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	return s, orch, ownerID
}

// session reads the one audit row back.
func session(t *testing.T, s *Service, ownerID string) (outcome string, ended bool, id uuid.UUID) {
	t.Helper()
	rows, err := s.q.ListExecSessions(context.Background(), dbgen.ListExecSessionsParams{OwnerID: ownerID, RowLimit: 10})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no exec session was recorded at all")
	}
	r := rows[0]
	return r.Outcome, r.EndedAt.Valid, r.ID
}

func sessionCount(t *testing.T, s *Service, ownerID string) int {
	t.Helper()
	rows, err := s.q.ListExecSessions(context.Background(), dbgen.ListExecSessionsParams{OwnerID: ownerID, RowLimit: 10})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return len(rows)
}

// PROBE 1 — the end row must survive the disconnect that caused it.
//
// This is the flagged seam. The tempting implementation writes the close inside
// the read loop or on the request context, and it passes a clean-exit test
// while losing the row for every session that ended the way sessions actually
// end: the client went away. So the assertion is made against a CANCELLED
// context — the state a real disconnect leaves — and it fails if the teardown
// depends on that context still being alive.
func TestADisconnectedSessionIsStillRecordedAsEnded(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-disc")

	ctx, cancel := context.WithCancel(context.Background())
	orch.onExec = func(execCtx context.Context, _ orchestrator.ExecSpec) error {
		// The client vanishes mid-session. This is what the WebSocket reader
		// goroutine does when ReadMessage fails.
		cancel()
		<-execCtx.Done()
		return execCtx.Err()
	}

	_ = s.Exec(ctx, ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"/bin/sh"}, Actor: "someone@example.test",
		Stdin: io.LimitReader(nil, 0),
	})

	outcome, ended, _ := session(t, s, ownerID)
	if !ended {
		t.Fatal("the session row was never closed — the teardown wrote through " +
			"a context the disconnect had already cancelled, which loses exactly " +
			"the sessions worth recording")
	}
	if outcome != ExecDisconnected {
		t.Errorf("outcome = %q, want %q — a shell killed by a network blip and "+
			"one that exited cleanly are different events", outcome, ExecDisconnected)
	}
}

// PROBE 2 — the row must exist, OPEN, while the session is still running.
//
// The companion the disconnect probe cannot surface, and the first version of
// this test did not surface it either: asserting that a failed attach leaves a
// row passes against a deferred write-everything-at-teardown implementation
// too, because the defer runs on that path as well. It looked like a probe and
// was not one.
//
// What write-at-teardown genuinely cannot produce is a row for a session that
// has not ended. That state is the whole design:
//
//   - while somebody is in a shell, it is visible that they are — a table that
//     only learns about sessions once they finish cannot answer "who is in a
//     container right now", which is the question an incident asks.
//   - and a row that is open forever is the signal for a session that ended in
//     a way this process never observed — a crash, a SIGKILL, a restart mid-
//     session. Write-at-teardown records nothing at all for those, because the
//     deferred write is exactly what did not happen.
//
// So the assertion is made from INSIDE the session, which is the only moment
// the two implementations differ.
func TestTheSessionIsVisibleWhileItIsStillRunning(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-open")

	var (
		seen   int
		open   bool
		lookup error
	)
	orch.onExec = func(context.Context, orchestrator.ExecSpec) error {
		// Mid-session: the shell is live and has not ended.
		rows, err := s.q.ListExecSessions(context.Background(),
			dbgen.ListExecSessionsParams{OwnerID: ownerID, RowLimit: 10})
		lookup = err
		seen = len(rows)
		if len(rows) > 0 {
			open = !rows[0].EndedAt.Valid
		}
		return nil
	}

	if err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"/bin/sh"}, Actor: "someone@example.test",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if lookup != nil {
		t.Fatalf("list during session: %v", lookup)
	}

	if seen != 1 {
		t.Fatalf("%d session rows visible DURING the session, want 1 — the row "+
			"must be written before the stream opens, or nobody can see who is "+
			"in a container right now, and a session killed with the process "+
			"leaves no trace at all", seen)
	}
	if !open {
		t.Error("the row was already closed while the session was still running")
	}

	// And it is closed afterwards, so an open row really does mean open.
	outcome, ended, _ := session(t, s, ownerID)
	if !ended || outcome != ExecExited {
		t.Errorf("after the session: ended = %v, outcome = %q", ended, outcome)
	}
}

// The open-row signal has to be READABLE, or it is a record kept for nobody.
//
// The partial index and the nullable column are only half the design; without a
// query for them, "who is in a container right now" is unanswerable and a
// session killed with the process is invisible rather than conspicuous.
func TestOpenSessionsAreQueryable(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-openq")

	var during []OpenSession
	var lookupErr error
	orch.onExec = func(context.Context, orchestrator.ExecSpec) error {
		during, lookupErr = s.OpenSessions(context.Background(), ownerID)
		return nil
	}

	if err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"psql"}, Actor: "alex@example.test",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if lookupErr != nil {
		t.Fatalf("OpenSessions during the session: %v", lookupErr)
	}

	if len(during) != 1 {
		t.Fatalf("%d open sessions while one was running, want 1", len(during))
	}
	if during[0].Actor != "alex@example.test" || during[0].Command != "psql" {
		t.Errorf("open session = %+v", during[0])
	}
	if during[0].AppName != "web" {
		t.Errorf("app = %q, want web", during[0].AppName)
	}

	// And it stops being open once it ends, so an open row really does mean a
	// session nobody has seen the end of.
	after, err := s.OpenSessions(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("OpenSessions: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d open sessions after the session ended: %+v", len(after), after)
	}
}

// A session that dies during attach is still recorded, with the reason.
func TestASessionThatDiesDuringAttachIsStillRecorded(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-attach")

	orch.onExec = func(context.Context, orchestrator.ExecSpec) error {
		return errors.New("the API server refused the upgrade")
	}

	err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"/bin/sh"}, Actor: "someone@example.test",
	})
	if err == nil {
		t.Fatal("a failed attach returned no error")
	}

	outcome, ended, _ := session(t, s, ownerID)
	if !ended {
		t.Error("the row was opened and never closed for a failure this process observed")
	}
	if outcome != ExecFailed {
		t.Errorf("outcome = %q, want %q", outcome, ExecFailed)
	}
}

// A clean exit records the code, which is what makes `oz exec` usable in a
// script. Included so the disconnect and failure cases are distinguishable from
// the ordinary one rather than merely from each other.
func TestACleanExitRecordsItsCode(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-clean")

	orch.onExec = func(context.Context, orchestrator.ExecSpec) error {
		return &orchestrator.ExitError{Code: 42}
	}

	err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"false"},
	})
	var exitErr *orchestrator.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 42 {
		t.Fatalf("err = %v, want an ExitError with code 42", err)
	}

	rows, qErr := s.q.ListExecSessions(context.Background(), dbgen.ListExecSessionsParams{OwnerID: ownerID, RowLimit: 10})
	if qErr != nil {
		t.Fatalf("list: %v", qErr)
	}
	r := rows[0]
	if r.Outcome != ExecExited {
		t.Errorf("outcome = %q, want %q", r.Outcome, ExecExited)
	}
	if r.ExitCode == nil || *r.ExitCode != 42 {
		t.Errorf("exit_code = %v, want 42", r.ExitCode)
	}
}

// The command is recorded, not just the fact of a session.
//
// "Somebody opened a shell" is an alert; "somebody ran psql" is an answer.
func TestTheSessionRecordsWhatWasRun(t *testing.T) {
	s, _, ownerID := execAuditService(t, "owner-exec-cmd")

	_ = s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"psql", "-c", "select 1"},
		Actor: "alex@example.test", TTY: true,
	})

	rows, err := s.q.ListExecSessions(context.Background(), dbgen.ListExecSessionsParams{OwnerID: ownerID, RowLimit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	r := rows[0]
	if r.Command != "psql -c select 1" {
		t.Errorf("command = %q", r.Command)
	}
	if r.Actor != "alex@example.test" {
		t.Errorf("actor = %q", r.Actor)
	}
	if r.Pod != "web-a" || !r.Tty {
		t.Errorf("pod = %q tty = %v", r.Pod, r.Tty)
	}
}

// A session cannot be opened into a pod that is not this app's, even by a
// caller that skipped ExecTargetFor — the guard is re-applied at the method
// that opens the connection.
func TestExecReChecksThePodItWasHanded(t *testing.T) {
	s, _, ownerID := execAuditService(t, "owner-exec-recheck")

	err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "someone-elses-pod-0", Command: []string{"/bin/sh"},
	})
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if n := sessionCount(t, s, ownerID); n != 0 {
		t.Errorf("%d session rows for a refused pod — nothing was opened", n)
	}
}

// An install whose orchestrator cannot exec refuses rather than recording a
// session that never happened.
func TestExecWithoutAnExecutorRefuses(t *testing.T) {
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-exec-noexec")
	if _, err := s.Create(context.Background(), ownerID, CreateInput{
		Name: "web", Image: "nginx:1", Port: 80, Replicas: 1,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := s.Exec(context.Background(), ownerID, "web", ExecInput{
		Pod: "web-a", Command: []string{"/bin/sh"},
	})
	if !errors.Is(err, ErrNoExec) {
		t.Errorf("err = %v, want ErrNoExec", err)
	}
}

// The teardown write is bounded as well as detached.
//
// Detached alone would hang the goroutine when the write races a database that
// has gone away — and on this path the client is already gone, so nothing else
// would ever notice. Asserted by giving the teardown a context that is already
// dead and requiring the call to return promptly.
func TestTheTeardownDoesNotHangOnADeadContext(t *testing.T) {
	s, orch, ownerID := execAuditService(t, "owner-exec-bound")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before the call

	orch.onExec = func(context.Context, orchestrator.ExecSpec) error { return nil }

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Exec(ctx, ownerID, "web", ExecInput{
			Pod: "web-a", Command: []string{"/bin/sh"},
		})
	}()

	select {
	case <-done:
	case <-time.After(execWriteTimeout + 5*time.Second):
		t.Fatal("Exec did not return — the teardown write is unbounded")
	}
}
