package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/identity"
)

// TokensPageData is the access-token surface.
//
// Issued carries a raw token exactly once, on the response to the request that
// minted it. It is not stored, not re-readable, and not in account.APIToken —
// which is why it has to be threaded through the page data rather than read
// back from the list below it.
type TokensPageData struct {
	TeamID   string
	TeamName string

	// BaseURL is what the copyable `oz auth login` line points at. Taken from
	// the install's configured public URL rather than from the request, because
	// the address that reached this dashboard may be a private one — an
	// ssh -L tunnel, or a hostname only resolvable inside the network — and a
	// command line that works for the person reading it and nowhere else is
	// worse than one that is obviously a placeholder.
	BaseURL string

	Tokens []account.APIToken

	// Issued is the token just minted, shown once. Empty on every other render.
	Issued string

	// IssuedName names it, so the one-time panel can say which of the rows in
	// the table the value belongs to.
	IssuedName string

	Error string
}

// tokenTTLs are the lifetimes the form offers.
//
// "No expiry" is first and is the default, because the common case is a
// credential in a CI secret and an expiry nobody wrote down is a pipeline that
// breaks on a date nobody remembers. The bounded options exist for the other
// case — a token pasted onto a laptop for an afternoon.
var tokenTTLs = []struct {
	Label string
	Days  int
}{
	{"No expiry", 0},
	{"30 days", 30},
	{"90 days", 90},
	{"1 year", 365},
}

// tokensPage lists the credentials the signed-in person holds in this team.
func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tokens, err := s.accounts.ListAPITokens(ctx, sess.UserID, owner.ID)
	if err != nil {
		s.log.Error("list api tokens", slog.String("error", err.Error()))
		http.Error(w, "could not load your tokens", http.StatusInternalServerError)
		return
	}

	s.render(w, r, TokensPage(TokensPageData{
		TeamID:   owner.ID,
		TeamName: displayName(owner),
		BaseURL:  s.tokenEndpoint(),
		Tokens:   tokens,
		Error:    r.URL.Query().Get("error"),
	}))
}

// tokenEndpoint is the address the copyable login line names.
//
// Falls back to a placeholder rather than to the request's own host. An install
// with no OZYMANDIS_BASE_URL set has not declared a public address, and printing
// whatever reached this handler would produce a command that works only from
// where the reader already is — localhost, or a tunnel — which is a command
// that fails confusingly on the machine it was copied to.
func (s *Server) tokenEndpoint() string {
	if s.baseURL != "" {
		return s.baseURL
	}
	return "https://your-install.example"
}

// tokenCreate mints one and shows it once.
//
// The raw value is rendered rather than redirected to, deliberately. A redirect
// would have to carry the token in the query string, which puts a live
// credential in the browser history, the access log of anything in front of
// this, and the Referer header of the next request the page makes. Rendering it
// on the response to the POST leaves it in exactly one place: the page in front
// of the person who asked for it.
func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	days, _ := strconv.Atoi(r.FormValue("ttl_days"))

	var ttl time.Duration
	if days > 0 {
		ttl = time.Duration(days) * 24 * time.Hour
	}

	raw, tok, err := s.accounts.IssueAPIToken(ctx, sess.UserID, owner.ID, name, ttl)
	if err != nil {
		s.renderTokens(w, r, sess.UserID, owner, "", "", tokenError(err))
		return
	}

	s.renderTokens(w, r, sess.UserID, owner, raw, tok.Name, "")
}

// tokenRevoke deletes one.
func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Redirect(w, r, "/settings/tokens?error=that+is+not+a+token+id", http.StatusSeeOther)
		return
	}

	// Scoped to this person and this team inside the service, so an id lifted
	// from somewhere else revokes nothing.
	if err := s.accounts.RevokeAPIToken(ctx, sess.UserID, owner.ID, id); err != nil {
		s.log.Error("revoke api token", slog.String("error", err.Error()))
		http.Redirect(w, r, "/settings/tokens?error=could+not+revoke+that+token",
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
}

// renderTokens re-reads the list and renders the page, carrying a one-time
// token or an error through.
func (s *Server) renderTokens(
	w http.ResponseWriter, r *http.Request,
	userID uuid.UUID, owner identity.Owner, issued, issuedName, errMsg string,
) {
	tokens, err := s.accounts.ListAPITokens(r.Context(), userID, owner.ID)
	if err != nil {
		s.log.Error("list api tokens", slog.String("error", err.Error()))
		http.Error(w, "could not load your tokens", http.StatusInternalServerError)
		return
	}
	s.render(w, r, TokensPage(TokensPageData{
		TeamID:     owner.ID,
		TeamName:   displayName(owner),
		BaseURL:    s.tokenEndpoint(),
		Tokens:     tokens,
		Issued:     issued,
		IssuedName: issuedName,
		Error:      errMsg,
	}))
}

// ttlValue is the form value for a lifetime in days.
func ttlValue(days int) string { return strconv.Itoa(days) }

// tokenLastUsed reports when a credential was last presented.
//
// "Never" rather than a date, because that is the fact that makes a token safe
// to delete — and rendering the zero time as 1 January year 1 would bury it.
func tokenLastUsed(t account.APIToken) string {
	if t.LastUsedAt == nil {
		return "Never"
	}
	return relativeTime(*t.LastUsedAt)
}

// tokenExpiry reports when a credential stops working.
func tokenExpiry(t account.APIToken) string {
	if t.ExpiresAt == nil {
		return "Never"
	}
	return shortTime(*t.ExpiresAt)
}

// tokenError turns a service error into something worth reading.
//
// The two a person can actually cause are named; everything else is generic,
// because the alternative is showing somebody a Postgres constraint.
func tokenError(err error) string {
	switch {
	case errors.Is(err, account.ErrTokenNameTaken):
		return "You already have a token with that name. " +
			"Names are unique so that revoking one cannot mean revoking the wrong one."
	case errors.Is(err, account.ErrNotAMember):
		return "You are not a member of this team."
	case err == nil:
		return ""
	default:
		return "That token could not be created. Give it a name and try again."
	}
}
