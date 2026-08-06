package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// TerminalSize is what the far end needs to know.
type TerminalSize struct {
	Cols, Rows uint16
}

// Injection points, so the restoration logic can be tested without a terminal.
//
// The property under test — that restore fires on every exit path, including a
// panic — has nothing to do with real termios and everything to do with control
// flow, and a test that needed a pty could not run in CI. These are variables
// rather than an interface because there is exactly one implementation and one
// fake, and an interface would be ceremony around a swap.
var (
	makeRawFn = func(fd int) (*term.State, error) { return term.MakeRaw(fd) }
	restoreFn = func(fd int, st *term.State) error { return term.Restore(fd, st) }
	sizeFn    = func(fd int) (int, int, error) { return term.GetSize(fd) }
	isTermFn  = func(fd int) bool { return term.IsTerminal(fd) }
)

// terminal owns the local end of an interactive session: raw mode, and the
// window-size notifications that go with it.
//
// # Why this is a type rather than a few defers in the command
//
// Two resources are acquired and both must be released on EVERY exit path, not
// just the tidy one. A shell left in raw mode has no echo and no line editing —
// the person's terminal is wrecked until they type `reset` blind — and that is
// what a panic, a write error, or an early return leaves behind if restoration
// is spelled out inline and one path misses it.
//
// It is the client-side mirror of the exec teardown on the server: works on
// clean exit, breaks on the failure path, and the failure path is the common
// one.
type terminal struct {
	fd int

	mu      sync.Mutex
	state   *term.State
	winch   chan os.Signal
	sizes   chan TerminalSize
	closed  bool
	stopped bool
}

func newTerminal(fd int) *terminal { return &terminal{fd: fd} }

// IsTerminal reports whether this is an interactive terminal at all.
//
// A piped stdin is not, and asking for raw mode on one fails — `oz exec` in a
// CI job is the ordinary case for that, so it is a question rather than an
// error.
func (t *terminal) IsTerminal() bool { return isTermFn(t.fd) }

// MakeRaw puts the terminal into raw mode and starts watching for resizes.
//
// The caller must defer Close, and Close must be deferred IMMEDIATELY after
// this returns — not after the next fallible step — or a failure in between
// leaves the terminal raw.
func (t *terminal) MakeRaw() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, err := makeRawFn(t.fd)
	if err != nil {
		return err
	}
	t.state = st

	// Buffered by one, and dropped rather than blocked on: a resize nobody read
	// yet is superseded by the next one, and a full channel must never stall
	// the signal runtime.
	t.winch = make(chan os.Signal, 1)
	t.sizes = make(chan TerminalSize, 1)
	signal.Notify(t.winch, syscall.SIGWINCH)

	go t.watchResizes()
	return nil
}

// watchResizes translates SIGWINCH into sizes until the channel closes.
func (t *terminal) watchResizes() {
	for range t.winch {
		cols, rows, err := sizeFn(t.fd)
		if err != nil {
			continue
		}
		select {
		case t.sizes <- TerminalSize{Cols: uint16(cols), Rows: uint16(rows)}:
		default:
			// The far end has not read the last one. Dropping is right: only
			// the most recent size matters, and blocking here would hold the
			// signal goroutine for the life of a slow session.
		}
	}
}

// Sizes carries window changes. Nil until MakeRaw.
func (t *terminal) Sizes() <-chan TerminalSize { return t.sizes }

// Size is the current size, for the initial resize sent at attach.
func (t *terminal) Size() (TerminalSize, bool) {
	cols, rows, err := sizeFn(t.fd)
	if err != nil {
		return TerminalSize{}, false
	}
	return TerminalSize{Cols: uint16(cols), Rows: uint16(rows)}, true
}

// Close restores the terminal and stops the signal handler.
//
// Idempotent, because it is deferred in more than one place on some paths and
// calling it twice must not double-restore or panic on a closed channel.
//
// signal.Stop is not optional bookkeeping. A notification registered and never
// stopped outlives the session: a process that runs `oz console` twice would
// have the second session's SIGWINCH delivered to the first session's channel
// as well, and the first goroutine is still reading it. That is the client-side
// form of "a guard only in the caller is one some caller skips" — a signal
// registered without a paired stop is a resource some exit path leaks.
func (t *terminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	if t.winch != nil && !t.stopped {
		signal.Stop(t.winch)
		close(t.winch) // ends watchResizes, which ends the goroutine
		t.stopped = true
	}

	if t.state == nil {
		return nil
	}
	st := t.state
	t.state = nil
	return restoreFn(t.fd, st)
}

// Raw reports whether the terminal is currently in raw mode.
//
// Exists for the tests that assert restoration happened, which is the whole
// point of this type.
func (t *terminal) Raw() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state != nil
}

// StoppedSignals reports whether the SIGWINCH registration was released.
func (t *terminal) StoppedSignals() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}
