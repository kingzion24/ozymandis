package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Exec is the interactive-session surface.
//
// Declared here like every other consumer interface. Session is deliberately
// the only method that opens one: ExecTarget answers "could I" without doing
// it, which is what lets a surface report why it cannot offer a console
// instead of finding out by failing.
type Exec interface {
	ExecTargetFor(ctx context.Context, ownerID, appName, wantPod string) (app.ExecTarget, error)
	Exec(ctx context.Context, ownerID, appName string, in app.ExecInput) error
	CanExec() bool
}

// upgrader turns the request into a WebSocket.
//
// gorilla rather than a second library: client-go's remotecommand already
// compiles gorilla into this binary for its own WebSocket transport, so using
// it here costs nothing, and adding a different one would put two
// implementations of the same protocol in one process.
//
// CheckOrigin refuses cross-origin upgrades. The browser sends no preflight for
// a WebSocket and its same-origin policy does not cover them, so without this
// any page a signed-in person visits could open a shell in their containers
// using their cookie. That is the one attack this endpoint uniquely enables,
// and the default gorilla ships with — allow everything — is the wrong one here.
var upgrader = websocket.Upgrader{
	HandshakeTimeout: 15 * time.Second,
	CheckOrigin:      sameOrigin,
}

// sameOrigin allows an upgrade only from this host, or from a client that sends
// no Origin at all.
//
// A CLI sends none: Origin is a browser concept, and a request without one
// cannot have been made by a page on somebody else's site. A browser always
// sends one, so a mismatch is a genuine cross-origin attempt.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// Control frames a client may send, as JSON text frames.
//
// Binary frames are stdin bytes and carry no framing of their own, which is
// what keeps the hot path free of encoding: a keystroke is a byte, not a
// base64 string inside an object.
type control struct {
	Resize *struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	} `json:"resize,omitempty"`
}

// exit is the final text frame, carrying what the command exited with.
type exit struct {
	Exit  int    `json:"exit"`
	Error string `json:"error,omitempty"`
}

// appExec opens a shell in one of an app's containers.
//
// Admin only. This is the most sensitive thing this platform can do — a shell
// in a production container reads every secret the app holds — and it is behind
// the same role that can delete the app.
func (s *Server) appExec(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	name := chi.URLParam(r, "name")

	var argv []string
	if raw := r.URL.Query()["cmd"]; len(raw) > 0 {
		argv = raw
	}
	if len(argv) == 0 {
		writeInvalid(w, "a command is required: ?cmd=/bin/sh")
		return
	}
	tty := r.URL.Query().Get("tty") == "true"

	// Resolved BEFORE the upgrade, so a refusal is an ordinary JSON error a
	// client can read. After the upgrade the status is spent and the only way
	// to report anything is a frame the client has to be listening for.
	target, err := s.exec.ExecTargetFor(r.Context(), owner.ID, name, r.URL.Query().Get("pod"))
	if err != nil {
		writeServiceError(w, s.log, "resolve exec target", err)
		return
	}
	if target.Pod == "" {
		writeError(w, http.StatusConflict, CodeUnavailable, target.Note)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own response.
		s.log.Warn("exec upgrade failed", slog.String("error", err.Error()))
		return
	}
	defer conn.Close()

	s.runExecSession(r, conn, owner.ID, name, target.Pod, argv, tty)
}

// runExecSession bridges the socket to the container.
func (s *Server) runExecSession(
	r *http.Request, conn *websocket.Conn,
	ownerID, name, pod string, argv []string, tty bool,
) {
	// The session outlives the request's own context in one direction only:
	// cancelling when the client goes away is exactly what should happen, so
	// this derives from the request rather than detaching. The TEARDOWN
	// detaches; the session does not.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stdinR, stdinW := io.Pipe()
	resize := make(chan orchestrator.TerminalSize, 1)

	// The reader goroutine owns closing both, so the writer side of the pipe
	// is closed exactly once and Exec's stdin sees a clean EOF rather than
	// hanging on a pipe nobody will write to again.
	go func() {
		defer close(resize)
		defer stdinW.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				// The client went away — a ^C, a closed laptop, a dropped
				// network. Cancelling ends the session, which is what makes
				// the teardown below run.
				cancel()
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				if _, err := stdinW.Write(data); err != nil {
					cancel()
					return
				}
			case websocket.TextMessage:
				var c control
				if err := json.Unmarshal(data, &c); err != nil || c.Resize == nil {
					continue
				}
				select {
				case resize <- orchestrator.TerminalSize{Cols: c.Resize.Cols, Rows: c.Resize.Rows}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	err := s.exec.Exec(ctx, ownerID, name, app.ExecInput{
		Pod:     pod,
		Command: argv,
		TTY:     tty,
		Stdin:   stdinR,
		Stdout:  &wsWriter{conn: conn},
		Stderr:  &wsWriter{conn: conn},
		Resize:  resize,
	})

	// Reported in-band as a final text frame: the status was spent at the
	// upgrade, so this is the only channel left. A client that has been reading
	// binary frames can read one more text frame; a socket that simply closed
	// would be indistinguishable from the shell exiting cleanly.
	final := exit{}
	var exitErr *orchestrator.ExitError
	switch {
	case errors.As(err, &exitErr):
		final.Exit = exitErr.Code
	case err != nil && ctx.Err() == nil:
		final.Exit, final.Error = -1, err.Error()
	}
	if data, mErr := json.Marshal(final); mErr == nil {
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// wsWriter sends container output as binary frames.
//
// Binary rather than text because container output is bytes, not necessarily
// valid UTF-8 — a program writing a control sequence or a partial multi-byte
// rune through a text frame is a protocol violation gorilla will refuse.
type wsWriter struct {
	conn *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
