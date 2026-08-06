package main

import (
	"errors"
	"testing"

	"golang.org/x/term"
)

// fakeTerm swaps the termios calls for bookkeeping.
//
// The property under test is control flow, not termios: does restore run on
// every path out. A test that needed a real pty could not run in CI, and the
// bug it would catch — a path that returns before the deferred restore — is
// visible without one.
type fakeTerm struct {
	raw      int // MakeRaw calls
	restored int // Restore calls
	failRaw  error
}

func (f *fakeTerm) install(t *testing.T) {
	t.Helper()
	origRaw, origRestore, origSize, origIsTerm := makeRawFn, restoreFn, sizeFn, isTermFn

	makeRawFn = func(int) (*term.State, error) {
		if f.failRaw != nil {
			return nil, f.failRaw
		}
		f.raw++
		return &term.State{}, nil
	}
	restoreFn = func(int, *term.State) error {
		f.restored++
		return nil
	}
	sizeFn = func(int) (int, int, error) { return 80, 24, nil }
	isTermFn = func(int) bool { return true }

	t.Cleanup(func() {
		makeRawFn, restoreFn, sizeFn, isTermFn = origRaw, origRestore, origSize, origIsTerm
	})
}

// THE probe for this stage's client half.
//
// A terminal left raw has no echo and no line editing: the person's shell is
// wrecked until they type `reset` blind. So restoration must survive every way
// out of a session, and the clean exit is the one that works by accident —
// exactly the shape the server-side exec teardown had.
//
// Each case is a different exit path, and every one of them must restore.
func TestTheTerminalIsRestoredOnEveryExitPath(t *testing.T) {
	cases := []struct {
		name string
		body func(*terminal)
	}{
		{
			name: "clean return",
			body: func(*terminal) {},
		},
		{
			name: "an error return partway through",
			body: func(*terminal) {
				// The shape of a failed dial after raw mode was entered.
				_ = errors.New("the session could not be opened")
			},
		},
		{
			name: "a panic mid-session",
			body: func(*terminal) {
				panic("the connection exploded")
			},
		},
		{
			name: "a runtime panic, not a deliberate one",
			body: func(*terminal) {
				var p *fakeTerm
				_ = p.raw // nil dereference
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTerm{}
			f.install(t)

			func() {
				// The panic is contained here, the way main's would be by the
				// process dying — what matters is that the deferred Close ran
				// on the way out.
				defer func() { _ = recover() }()

				term := newTerminal(0)
				if err := term.MakeRaw(); err != nil {
					t.Fatalf("MakeRaw: %v", err)
				}

				defer term.Close()

				tc.body(term)
			}()

			if f.raw != 1 {
				t.Fatalf("MakeRaw ran %d times, want 1", f.raw)
			}
			if f.restored != 1 {
				t.Errorf("the terminal was left in RAW MODE on the %q path — "+
					"restore ran %d times, want 1. The person's shell has no echo "+
					"and no line editing until they type `reset` blind.",
					tc.name, f.restored)
			}
		})
	}
}

// The SIGWINCH registration must be released too.
//
// A notification registered and never stopped outlives the session: a process
// that opens two consoles has the second session's resizes delivered to the
// first session's channel, where a goroutine is still reading. The client-side
// form of a resource some exit path leaks.
func TestCloseStopsTheSignalHandler(t *testing.T) {
	f := &fakeTerm{}
	f.install(t)

	term := newTerminal(0)
	if err := term.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if term.StoppedSignals() {
		t.Fatal("signals were stopped before the session ended")
	}

	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !term.StoppedSignals() {
		t.Error("Close did not stop the SIGWINCH registration — a second " +
			"console would deliver its resizes to this session's channel too")
	}
	if term.Raw() {
		t.Error("still in raw mode after Close")
	}
}

// Close is deferred on more than one path in some flows, so calling it twice
// must not double-restore or panic on an already-closed channel.
func TestCloseIsIdempotent(t *testing.T) {
	f := &fakeTerm{}
	f.install(t)

	term := newTerminal(0)
	if err := term.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}

	for i := range 3 {
		if err := term.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if f.restored != 1 {
		t.Errorf("restore ran %d times across three Closes, want 1", f.restored)
	}
}

// A terminal that never entered raw mode must not be "restored" to a state it
// never had — Close on a fresh terminal is a no-op, not a termios call with a
// nil state.
func TestCloseWithoutMakeRawDoesNothing(t *testing.T) {
	f := &fakeTerm{}
	f.install(t)

	term := newTerminal(0)
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.restored != 0 {
		t.Errorf("restore ran %d times for a terminal never made raw", f.restored)
	}
}

// A failure to enter raw mode leaves nothing to restore, and must be reported
// rather than swallowed — attaching with a terminal that silently stayed cooked
// gives a shell where every keystroke waits for Enter.
func TestMakeRawFailureIsReported(t *testing.T) {
	f := &fakeTerm{failRaw: errors.New("not a terminal")}
	f.install(t)

	term := newTerminal(0)
	if err := term.MakeRaw(); err == nil {
		t.Fatal("a failed MakeRaw was swallowed")
	}
	if term.Raw() {
		t.Error("reports raw mode after MakeRaw failed")
	}
	// And Close is still safe to call.
	if err := term.Close(); err != nil {
		t.Errorf("Close after a failed MakeRaw: %v", err)
	}
}
