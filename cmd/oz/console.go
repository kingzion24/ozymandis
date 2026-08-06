package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

func init() {
	register(&command{
		name:    "console",
		usage:   "console [--app N] [--pod P]",
		summary: "Open a shell in a running container",
		run:     runConsole,
	})
	register(&command{
		name:    "exec",
		usage:   "exec [--app N] -- CMD [ARGS…]",
		summary: "Run one command in a running container",
		run:     runExec,
	})
}

// shellFallbacks are tried in order when nobody names a command.
//
// bash first because it is what a person expects, sh second because it is what
// a minimal image has. Tried by the CLI rather than the server: the server
// cannot know what is in the image, and a failed exec is cheap.
var shellFallbacks = []string{"/bin/bash", "/bin/sh"}

func runConsole(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz console", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	pod := fs.String("pod", "", "which replica (default: the lowest-named ready one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	// Each shell in turn. A container with no bash is ordinary, and the failure
	// it produces — the command not existing — is indistinguishable at this
	// layer from the session failing, so the fallback is tried rather than
	// reported.
	var lastErr error
	for i, sh := range shellFallbacks {
		err := attach(ctx, env, name, *pod, []string{sh}, true)
		if err == nil {
			return nil
		}
		var exitErr *remoteExit
		if errors.As(err, &exitErr) {
			// The shell ran and exited. That is the session working.
			return exitErr
		}
		lastErr = err
		if i < len(shellFallbacks)-1 {
			fmt.Fprintf(env.Err, "no %s in this container, trying %s…\n",
				sh, shellFallbacks[i+1])
		}
	}
	return lastErr
}

func runExec(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz exec", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	pod := fs.String("pod", "", "which replica (default: the lowest-named ready one)")
	tty := fs.Bool("tty", false, "allocate a terminal (default: only if stdin is one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("oz: run what? Try `oz exec -- ls -la`")
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	// A TTY when stdin is one, unless asked otherwise. `oz exec -- ls` in a CI
	// job must not allocate a terminal: the output would carry control
	// sequences into a log nobody can read.
	wantTTY := *tty || newTerminal(int(os.Stdin.Fd())).IsTerminal()

	return attach(ctx, env, name, *pod, fs.Args(), wantTTY)
}

// remoteExit is a command that ran and exited non-zero.
type remoteExit struct{ Code int }

func (e *remoteExit) Error() string {
	return fmt.Sprintf("the command exited %d", e.Code)
}

// ExitCode is what `oz` should exit with, so a script sees what the command saw.
func (e *remoteExit) ExitCode() int { return e.Code }

// attach opens a session and pumps it until it ends.
//
// # Restoration
//
// The terminal is restored on EVERY exit path — clean exit, a write error, a
// cancelled context, a panic. `defer t.Close()` sits immediately after MakeRaw
// and before anything else that can fail, which is the whole discipline: a
// terminal left raw has no echo and no line editing, and the person has to type
// `reset` blind to get their shell back.
func attach(
	ctx context.Context, env *Env, name, pod string, argv []string, tty bool,
) error {
	t := newTerminal(int(os.Stdin.Fd()))
	if tty && t.IsTerminal() {
		if err := t.MakeRaw(); err != nil {
			return fmt.Errorf("oz: could not put this terminal into raw mode: %w", err)
		}
		// IMMEDIATELY. Everything after this point can fail, and all of it must
		// still hand the terminal back.
		defer t.Close()
	}

	conn, err := env.Client.Exec(ctx, name, pod, argv, tty)
	if err != nil {
		return err
	}
	defer conn.Close()

	if tty {
		if size, ok := t.Size(); ok {
			_ = sendResize(conn, size)
		}
		go pumpResizes(ctx, conn, t.Sizes())
	}

	go pumpStdin(conn)

	return pumpOutput(env, conn)
}

// pumpStdin sends keystrokes as binary frames.
func pumpStdin(conn *websocket.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if wErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); wErr != nil {
				return
			}
		}
		if err != nil {
			// EOF on stdin is a ^D, which the far end should see as one. The
			// close frame tells it; simply stopping would leave the shell
			// waiting for input that will never come.
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

func pumpResizes(ctx context.Context, conn *websocket.Conn, sizes <-chan TerminalSize) {
	for {
		select {
		case <-ctx.Done():
			return
		case size, ok := <-sizes:
			if !ok {
				return
			}
			if err := sendResize(conn, size); err != nil {
				return
			}
		}
	}
}

func sendResize(conn *websocket.Conn, size TerminalSize) error {
	msg := map[string]any{
		"resize": map[string]uint16{"cols": size.Cols, "rows": size.Rows},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// pumpOutput writes container output to stdout until the session ends.
//
// The final text frame carries the exit code. A socket that simply closed would
// be indistinguishable from the shell exiting cleanly, which is why the server
// sends one and this waits for it.
func pumpOutput(env *Env, conn *websocket.Conn) error {
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return nil
			}
			// An abnormal close is the session dying, not the command failing.
			// Said plainly rather than as a websocket error code.
			return errors.New("oz: the session ended unexpectedly — " +
				"the container may have restarted")
		}

		switch kind {
		case websocket.BinaryMessage:
			if _, wErr := env.Out.Write(data); wErr != nil {
				return wErr
			}
		case websocket.TextMessage:
			var final struct {
				Exit  int    `json:"exit"`
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &final) != nil {
				continue
			}
			if final.Error != "" {
				return errors.New("oz: " + final.Error)
			}
			if final.Exit != 0 {
				return &remoteExit{Code: final.Exit}
			}
			return nil
		}
	}
}

// Exec dials the console endpoint.
func (c *Client) Exec(
	ctx context.Context, name, pod string, argv []string, tty bool,
) (*websocket.Conn, error) {
	u, err := url.Parse(c.endpoint + "/api/v1/apps/" + name + "/exec")
	if err != nil {
		return nil, fmt.Errorf("oz: %w", err)
	}
	// ws:// and wss:// rather than http(s), which is what the dialer expects.
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	q := u.Query()
	for _, a := range argv {
		q.Add("cmd", a)
	}
	if pod != "" {
		q.Set("pod", pod)
	}
	if tty {
		q.Set("tty", "true")
	}
	u.RawQuery = q.Encode()

	// The token, and deliberately no Origin: this is not a browser, nothing
	// ambient is attached, and the server treats a missing Origin as safe for
	// exactly that reason.
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.token)

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			// The refusal arrived before the upgrade, so it is an ordinary JSON
			// error with something useful in it — "this app is scaled to zero",
			// most usefully. Reported as that rather than as "bad handshake".
			defer resp.Body.Close()
			return nil, decodeError(resp)
		}
		return nil, fmt.Errorf("oz: could not open a session: %w", err)
	}
	return conn, nil
}

// exitCodeOf reports the code a command exited with, for main to exit on.
func exitCodeOf(err error) (int, bool) {
	var e *remoteExit
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}

var _ = io.Discard
var _ = strings.TrimSpace
