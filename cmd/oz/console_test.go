package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// What the CLI says when stdin runs out decides whether a scripted exec works
// at all.
//
// Saying it with a close frame is indistinguishable, at the far end, from the
// person closing their laptop — so the server ended the session and killed the
// command. Every `oz exec -- ls` printed nothing and exited 0. The end of input
// has to be said in a frame that means only that, and the socket has to stay
// open afterwards to carry the output and the exit code back.
func TestEndOfStdinIsSaidWithoutClosingTheSocket(t *testing.T) {
	type frame struct {
		kind int
		data string
	}
	frames := make(chan frame, 4)

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				// A close arrives here as an error, which is exactly the
				// behaviour under test: report it rather than swallow it.
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					frames <- frame{kind: websocket.CloseMessage}
				}
				close(frames)
				return
			}
			frames <- frame{kind: kind, data: string(data)}
		}
	}))
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// stdin already at its end, which is every non-interactive invocation.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved }()

	pumpStdin(&connWriter{conn: conn})

	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("the socket was closed at the end of stdin — " +
				"the command is killed before it can answer")
		}
		if f.kind == websocket.CloseMessage {
			t.Fatal("stdin ended with a close frame — the server reads that " +
				"as the client leaving and cancels the session")
		}
		if f.kind != websocket.TextMessage || f.data != `{"stdin":"eof"}` {
			t.Fatalf("frame = (%d, %q), want a text {\"stdin\":\"eof\"}", f.kind, f.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was sent when stdin ended; the far end waits forever")
	}
}
