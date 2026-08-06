package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/kingzion24/ozymandis/internal/app"
)

// Secret is one environment entry.
//
// There is no Value field, and that is the point. A sealed value has no read
// path anywhere in this codebase — the dashboard cannot show one, and the app
// service only opens them on the way to the cluster — and an API is exactly
// where that rule would be quietly broken, because "the CLI needs to read it
// back" sounds reasonable right up until a token in CI can dump every
// credential the install holds.
//
// Plaintext values are returned, because they are already readable on the app
// detail page and pretending otherwise would be theatre rather than secrecy.
type Secret struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret"`
}

func (s *Server) secretList(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	out := make([]Secret, 0, len(a.Variables))
	for _, v := range a.Variables {
		// v.Value is already empty for a sealed variable — the service blanks
		// it in variablesFor rather than trusting each caller to. Copied
		// through as-is rather than re-checked, so there is one rule and not
		// two that must agree.
		out = append(out, Secret{Key: v.Key, Value: v.Value, Secret: v.Secret})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	writeJSON(w, s.log, http.StatusOK, map[string]any{"variables": out})
}

// SetSecrets is the body of PUT /apps/{name}/secrets.
//
// Additive, not declarative: it sets the keys named and leaves the rest alone.
// Deleting is its own verb on its own URL, because a PUT that removed every
// key it did not mention would make `oz secrets set ONE=1` an outage.
type SetSecrets struct {
	// Secret marks these values as sealed. Defaults to true: a caller writing
	// through this endpoint is far more often setting a credential than a log
	// level, and the safe default is the one that seals.
	Secret *bool `json:"secret,omitempty"`

	Variables map[string]string `json:"variables"`
}

func (s *Server) secretSet(w http.ResponseWriter, r *http.Request) {
	var in SetSecrets
	if err := decodeJSON(r, &in); err != nil {
		writeInvalid(w, "that body is not the JSON this expects: "+err.Error())
		return
	}
	if len(in.Variables) == 0 {
		writeInvalid(w, "variables is required and must not be empty")
		return
	}

	sealed := true
	if in.Secret != nil {
		sealed = *in.Secret
	}

	owner := ownerOf(r)
	name := chi.URLParam(r, "name")

	// Sorted so that a partial failure is at least deterministic: the same
	// request against the same state fails at the same key, rather than at
	// whichever one map iteration reached first.
	keys := make([]string, 0, len(in.Variables))
	for k := range in.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		err := s.apps.SetVariable(r.Context(), owner.ID, name, app.VariableInput{
			Key: k, Value: in.Variables[k], Secret: sealed,
		})
		if err != nil {
			// Named, so a failure halfway through a batch says which key it
			// stopped at. The ones before it are written; there is no
			// transaction spanning them, and claiming otherwise would be worse
			// than saying where it stopped.
			writeServiceError(w, s.log, "set variable "+k, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) secretDelete(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		writeInvalid(w, "a key is required")
		return
	}
	err := s.apps.DeleteVariable(r.Context(), ownerOf(r).ID, chi.URLParam(r, "name"), key)
	if err != nil {
		writeServiceError(w, s.log, "delete variable", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
