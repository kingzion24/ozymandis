package orchestrator

import (
	"context"
	"errors"
	"io"
)

// TerminalSize is how big the terminal on the other end is.
type TerminalSize struct {
	Cols, Rows uint16
}

// ExecSpec is one interactive session against one container.
type ExecSpec struct {
	Ref

	// Pod is which container to attach to, and it is REQUIRED.
	//
	// Resolved by the caller rather than chosen here, and that is the security
	// boundary rather than a convenience: a pod name is enough to reach any
	// tenant's container, so deciding which pod is a decision that must happen
	// in the owner-scoped layer. An implementation that picked a pod itself
	// would be picking from every pod in a namespace it was handed, with no
	// way to know whose app it belongs to.
	//
	// This mirrors resolveLogTarget, which exists for the same reason and is
	// the only place a log read decides what it may touch.
	Pod string

	// Command is the whole argv. No shell runs between this and the process
	// unless the command names one.
	Command []string

	// TTY allocates a pseudo-terminal, which is what makes an interactive
	// shell behave like one — line editing, job control, and a program that
	// checks isatty deciding it has a human in front of it.
	//
	// It also merges stderr into stdout, because a terminal has one stream.
	// A caller that needs them apart must not ask for a TTY.
	TTY bool

	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// Resize carries terminal size changes for the life of the session. Nil is
	// fine and means the size never changes, which is right for a
	// non-interactive exec.
	//
	// Without it a full-screen program draws for whatever size the terminal
	// was when the session opened and never notices the window moving, which
	// is the difference between `top` being usable and being wallpaper.
	Resize <-chan TerminalSize
}

// ErrNoPod means the app has no container that can be attached to.
//
// Distinct from a missing app: the app exists and this is a statement about
// what it is doing right now. The caller turns it into something a person can
// act on, which needs the reason — scaled to zero, mid-deploy, crashlooping —
// and that reason lives with the pods rather than here.
var ErrNoPod = errors.New("orchestrator: no attachable pod")

// ExitError is a session whose command ended non-zero.
//
// A typed error rather than a status returned alongside nil, so a caller that
// forgets to check gets a failure rather than a silent success. `oz exec` exits
// with this code, which is what makes it usable in a script.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return "orchestrator: the command exited " + itoa(e.Code)
}

// itoa avoids importing strconv for one call in an error path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Executor opens an interactive session inside a running container.
//
// Optional, and asserted for rather than required, in the same way Runner and
// NodeManager are. Everything else this engine does can be served by a
// credential that reaches the API server for reads and writes to workloads;
// exec needs the pods/exec subresource specifically, which an install may
// reasonably not grant. Such an install leaves the surface off with a reason
// rather than offering a console that fails when somebody presses it.
type Executor interface {
	// Exec runs Command in spec.Pod and blocks until it ends.
	//
	// It returns *ExitError when the command ran and exited non-zero, which is
	// not a failure of this method — the session worked and the program said
	// no. Any other error means the session itself could not be established or
	// did not survive.
	Exec(ctx context.Context, spec ExecSpec) error
}
