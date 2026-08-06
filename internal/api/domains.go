package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Domain is a hostname somebody brought.
type Domain struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Verified bool   `json:"verified"`

	// Target is what the CNAME has to point at. Carried because the whole
	// reason to list domains from a CLI is to be told what DNS record to
	// create, and a host with no target is a claim nobody can act on.
	Target string `json:"target,omitempty"`
}

// domainList returns the app's custom hostnames.
//
// Its own endpoint rather than a field on the app, because domains are the axis
// ozymandis.toml does not own: the file is additive and cannot express removal,
// so the CLI needs somewhere to see the real set and somewhere to remove from.
// Without this pair, "left alone and reported" would be a report with no
// corresponding action.
func (s *Server) domainList(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	net, err := s.nets.Networking(r.Context(), ownerOf(r).ID, a.Name)
	if err != nil {
		writeServiceError(w, s.log, "read networking", err)
		return
	}

	out := make([]Domain, 0, len(net.Custom))
	for _, c := range net.Custom {
		out = append(out, Domain{
			ID: c.ID.String(), Host: c.Host, Verified: c.Verified, Target: c.Target,
		})
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{
		"domains": out,
		// The platform-issued hostname, which is not a custom domain and cannot
		// be removed, but is what somebody asking "where is my app" wants.
		"managed": net.Managed,
		"target":  net.Target,
	})
}

// AddDomain is the body of POST /apps/{name}/domains.
type AddDomain struct {
	Host string `json:"host"`
}

func (s *Server) domainAdd(w http.ResponseWriter, r *http.Request) {
	var in AddDomain
	if err := decodeJSON(r, &in); err != nil {
		writeInvalid(w, "that body is not the JSON this expects: "+err.Error())
		return
	}
	if in.Host == "" {
		writeInvalid(w, "host is required")
		return
	}

	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	if err := s.nets.AddDomain(r.Context(), ownerOf(r).ID, a.Name, in.Host); err != nil {
		writeServiceError(w, s.log, "add domain", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// domainRemove drops a hostname.
//
// The only way to remove one, deliberately: ozymandis.toml is additive, so
// deleting a line from it does nothing. Removal is an explicit act because it
// takes a certificate with it and starts returning NXDOMAIN to live traffic —
// which is not what somebody tidying up a config file means to do.
func (s *Server) domainRemove(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeInvalid(w, "that is not a domain id")
		return
	}

	if err := s.nets.RemoveDomain(r.Context(), ownerOf(r).ID, a.Name, id); err != nil {
		writeServiceError(w, s.log, "remove domain", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
