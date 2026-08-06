package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kingzion24/ozymandis/internal/app"
)

// LogLine is one line of container output.
type LogLine struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// appLogs serves an app's output, either as a page or as a stream.
//
// One endpoint with a follow parameter rather than two, because the two differ
// only in whether the response ends. A client that wants to tail sets
// follow=true and reads until it stops; everything else about the request —
// which pod, how far back — is identical, and splitting them would mean two
// places to keep those options in agreement.
func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	name := chi.URLParam(r, "name")
	q := r.URL.Query()

	// The app is resolved by owner before anything is streamed, even though
	// app.Service.Logs scopes by owner itself. Logs is an interface here, and
	// an implementation that forgot to scope would turn this endpoint into a
	// way to read another team's output — a guarantee the interface does not
	// state is a guarantee this handler must not depend on.
	//
	// It also fixes what a missing app looks like: without this, a name nobody
	// owns reaches the log service and comes back as whatever that makes of it,
	// rather than as the 404 every other endpoint here returns.
	if _, err := s.lookup(r); err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	req := app.LogRequest{
		Pod:      q.Get("pod"),
		Previous: q.Get("previous") == "true",
		Tail:     200,
	}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 10000 {
			writeInvalid(w, "tail must be a number between 1 and 10000")
			return
		}
		req.Tail = n
	}

	if q.Get("follow") == "true" {
		s.streamLogs(w, r, owner.ID, name, req)
		return
	}

	logs, err := s.logs.Logs(r.Context(), owner.ID, name, req)
	if err != nil {
		writeServiceError(w, s.log, "read logs", err)
		return
	}

	lines := make([]LogLine, 0, len(logs.Lines))
	for _, l := range logs.Lines {
		lines = append(lines, LogLine{At: l.At, Text: l.Text})
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{
		"pod": logs.Pod, "pods": logs.Pods, "lines": lines, "note": logs.Note,
	})
}

// streamLogs follows the container until the client goes away.
//
// NDJSON — one JSON object per line — rather than Server-Sent Events, which is
// what the dashboard uses. SSE exists to be consumed by EventSource in a
// browser; a CLI reading it would be stripping "data: " prefixes off every
// line for no benefit. NDJSON is what every other tool in this space emits and
// what `jq` reads without arguments.
func (s *Server) streamLogs(
	w http.ResponseWriter, r *http.Request, ownerID, name string, req app.LogRequest,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, every line sits in a buffer until the response ends
		// — and this response does not end. Refusing is honest; a stream that
		// silently delivers nothing is not.
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"this server cannot stream")
		return
	}

	seq, err := s.logs.LogStream(r.Context(), ownerID, name, req)
	if err != nil {
		writeServiceError(w, s.log, "stream logs", err)
		return
	}

	// Headers before the first line, because after that the status is already
	// sent and an error has nowhere to go but the body.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for line, err := range seq {
		if err != nil {
			// The status is long gone, so this is reported in-band as a final
			// object. A client that has been parsing objects can parse one
			// more; truncating the stream silently would look like the app
			// exiting cleanly.
			_ = enc.Encode(map[string]string{"error": err.Error()})
			flusher.Flush()
			return
		}
		if err := enc.Encode(LogLine{At: line.At, Text: line.Text}); err != nil {
			// The client went away. Returning ends the iterator, which is what
			// closes the connection this handler is holding open to the
			// cluster — leaving it running would leak one per abandoned tail.
			return
		}
		flusher.Flush()
	}
}
