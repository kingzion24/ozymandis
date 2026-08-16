// Package api is the JSON surface machines drive Ozymandis through.
//
// A separate package from web rather than more handlers inside it, because the
// two answer to different callers and the difference shows up in every failure
// path. The dashboard's job on an error is to render a page a person can act
// on; this one's is to return a status and a code a script can branch on, and
// a handler trying to do both does neither well — the concrete symptom being
// a CLI that receives a 200 carrying an HTML sign-in form.
//
// What the two share is everything that matters for correctness: the same
// identity.Provider resolves the owner, the same app.Service does the work, and
// the same role rule decides who may do it. Nothing here is a second
// implementation of anything; it is a second encoding.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/domain"
)

// Error is the body of every failed response.
//
// One shape for all of them, so a client has one thing to parse. Code is a
// stable machine-readable string and Message is for a human reading a terminal;
// clients branch on the former, and the latter is free to be reworded.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Error Error `json:"error"`
}

// Error codes. Stable: a client may branch on these, so they are part of the
// interface in the way the prose of a message is not.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeInvalid         = "invalid"
	CodeConflict        = "conflict"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
)

// writeError sends a failure.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The response is a fixed-shape struct with no user data in a position that
	// could fail to encode, so an error here means the client went away.
	_ = json.NewEncoder(w).Encode(errorBody{Error{Code: code, Message: msg}})
}

// writeJSON sends a success.
//
// The body is encoded into memory before anything is written, so that a
// failure to marshal produces a 500 with a JSON body rather than a 200 whose
// body stops mid-object. A client parsing the latter reports a syntax error at
// some byte offset, which is a long way from the actual fault.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		log.Error("encode response", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"the response could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeServiceError maps a service error onto a status and a code.
//
// Centralised so that every handler reports the same failure the same way.
//
// The unrecognised case is a 500, which is a deliberate departure from the
// dashboard: appActionFailed treats everything but ErrNotFound as 422, and
// that is right for a page, because a person reading a message does not care
// which side the fault was on. A machine does. A 422 says "your request is
// wrong, do not send it again", so a CI job that hits a database outage would
// report a broken deploy and stop, where a 500 would be retried and succeed.
//
// The cost of choosing 500 as the default is the mirror image — a genuinely
// bad request retried a few times before failing — and that is the cheaper
// mistake. It is also mostly avoided rather than merely accepted: handlers
// validate what they can before calling the service, so the errors that reach
// here uncategorised really are the ones nobody anticipated.
//
// THIS SWITCH IS A REGISTRATION POINT. Adding a sentinel to the app package
// does not make it reportable here — it lands in the default and becomes a 500.
// That is the deliberate direction, but it is not free: a new
// ErrQuotaExceeded reported as 500 is one a client retries forever rather than
// surfacing to whoever has to raise the quota. If you add an error to the app
// package that a caller could act on, add it here too.
func writeServiceError(w http.ResponseWriter, log *slog.Logger, op string, err error) {
	switch {
	case errors.Is(err, app.ErrNotFound),
		errors.Is(err, app.ErrVolumeNotFound),
		errors.Is(err, app.ErrVariableNotFound),
		errors.Is(err, app.ErrProjectNotFound),
		errors.Is(err, app.ErrTemplateNotFound),
		errors.Is(err, domain.ErrDomainNotFound):
		// 404 rather than a description of what is missing. See notFound.
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())

	case errors.Is(err, app.ErrNameTaken):
		writeError(w, http.StatusConflict, CodeConflict,
			"an app with that name already exists")

	case errors.Is(err, domain.ErrHostTaken):
		// The same shape as ErrNameTaken: the name is well formed and somebody
		// else has it. Retrying is pointless; picking another name is not.
		writeError(w, http.StatusConflict, CodeConflict, err.Error())

	case errors.Is(err, domain.ErrHostReserved),
		errors.Is(err, domain.ErrNotVerified):
		// Both are the caller's to fix and neither is a malformed request:
		// one names something the platform issues itself, the other a name
		// whose DNS does not point here yet. 422 rather than 400 for the same
		// reason ErrVolumeShrink is — the syntax was fine, the ask was not.
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, err.Error())

	case errors.Is(err, app.ErrVolumeAttached):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())

	case errors.Is(err, app.ErrVolumeShrink):
		writeError(w, http.StatusUnprocessableEntity, CodeInvalid, err.Error())

	case errors.Is(err, app.ErrNoBuilder),
		errors.Is(err, app.ErrNoSecretKey),
		errors.Is(err, app.ErrSourceUnavailable),
		errors.Is(err, app.ErrNoExec),
		errors.Is(err, app.ErrNoRunner),
		errors.Is(err, domain.ErrNoTarget),
		errors.Is(err, domain.ErrNoAppDomain):
		// None of these is a fault in the request. They are all the same shape:
		// the caller asked for something reasonable and THIS INSTALL is not
		// configured for it — no registry, no secret key, no source, no exec, no
		// task runner, no CNAME target, no app domain. A 4xx would send somebody
		// to re-read their own arguments for a problem that is in the install's
		// configuration, and a 500 would have a client retry a missing registry
		// forever.
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())

	case errors.Is(err, account.ErrNotAMember):
		writeError(w, http.StatusForbidden, CodeForbidden,
			"you are not a member of this team")

	default:
		// Logged in full, reported in outline. The detail of an internal
		// failure is for the operator's log, not for whoever sent the request.
		log.Error(op, slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"something went wrong here; check the server log")
	}
}

// writeInvalid reports a request this package rejected before the service saw
// it.
//
// Separate from writeServiceError so the two cannot be confused: this one is
// always the caller's fault and always safe to report verbatim, because the
// message came from a validator rather than from a database.
func writeInvalid(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, CodeInvalid, msg)
}

// notFound is the answer for a resource that does not exist OR that the caller
// may not see.
//
// Deliberately the same response for both. A 403 on somebody else's app
// confirms that the app exists, which turns this endpoint into a way to
// enumerate another team's app names one guess at a time. Scoping every query
// by owner already means a stranger's app simply is not there, and this keeps
// the response consistent with that.
func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, CodeNotFound, "no such app")
}

// decodeJSON reads a request body into v.
//
// Unknown fields are rejected rather than ignored. A misspelled field in a
// PUT is a caller believing they set something they did not, and silently
// accepting it means the deploy goes out with the old value and nothing
// anywhere says so.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// maxBodyBytes caps a request body.
//
// Generous for a config document and nowhere near enough to be a memory
// problem. It exists because every handler here decodes before it authorises
// anything about the content, and an unbounded read is a way to spend this
// process's memory with one request.
const maxBodyBytes = 1 << 20 // 1 MiB
