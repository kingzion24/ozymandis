package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/account"
)

// Viewer is the person behind this request, as the chrome needs them.
//
// Deliberately not part of identity.Owner. That type answers one question —
// who owns this request — and everything downstream takes an OwnerID without
// asking where it came from. Two of the three identity configurations have no
// person at all: a bearer-token install acts as the owner, and an install with
// no token is whoever reached the port. Putting a user on Owner would add a
// field that is empty in both, and would make a seam whose whole value is not
// knowing how identity was established start knowing.
//
// So this is a web concept, resolved from the session cookie, absent where
// there is no session, and read only by the chrome.
type Viewer struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	IsSuperuser bool
}

// Label is what to call this person on screen.
func (v Viewer) Label() string {
	if v.DisplayName != "" {
		return v.DisplayName
	}
	return v.Username
}

// Sub is the second line under the label, or empty.
//
// The username when a display name is showing, because the username is what
// they sign in with and what an administrator refers to them by. Otherwise the
// one thing about them worth stating without being asked.
func (v Viewer) Sub() string {
	if v.DisplayName != "" && v.Username != v.DisplayName {
		return v.Username
	}
	if v.IsSuperuser {
		return "superuser"
	}
	return ""
}

// viewerKey addresses the viewer on the request. An unexported struct type, so
// nothing outside this package can plant a person on a context and have the
// chrome present them as signed in.
type viewerKey struct{}

// viewerLookup defers the resolution until something actually renders chrome.
type viewerLookup func() (Viewer, bool)

// ViewerFromContext returns the person this request's session belongs to.
//
// Absent on an install with no account service, and on a request with no
// session — a bearer token identifies an owner, not a person, and there is
// nobody to name.
func ViewerFromContext(ctx context.Context) (Viewer, bool) {
	look, _ := ctx.Value(viewerKey{}).(viewerLookup)
	if look == nil {
		return Viewer{}, false
	}
	return look()
}

// withViewer makes the signed-in person available to whatever renders chrome.
//
// Deferred rather than resolved here, for the reason withTeams is: it costs two
// queries, and most requests through this group are POSTs that redirect and
// never draw a sidebar. Only the responses that show it pay for it.
func (s *Server) withViewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			once   sync.Once
			viewer Viewer
			ok     bool
		)
		look := viewerLookup(func() (Viewer, bool) {
			once.Do(func() { viewer, ok = s.viewerFor(r) })
			return viewer, ok
		})
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, look)))
	})
}

// viewerFor resolves the person from the presented cookie and nothing else.
//
// The session says who they are; the row says what to call them. A viewer built
// from anything the request asserted would be a name the caller chose to have
// displayed as themselves.
func (s *Server) viewerFor(r *http.Request) (Viewer, bool) {
	if s.accounts == nil {
		return Viewer{}, false
	}
	ctx := r.Context()

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		// A request with no cookie is the ordinary case on the sign-in page, and
		// is not worth a line in the log.
		if !errors.Is(err, account.ErrSessionInvalid) {
			s.log.Error("resolve session for the chrome", slog.String("error", err.Error()))
		}
		return Viewer{}, false
	}

	u, err := s.accounts.User(ctx, sess.UserID)
	if err != nil {
		// The chrome loses a name rather than the page failing. A dashboard that
		// 500s because one label could not be drawn is a worse answer than a
		// dashboard that says "Account".
		s.log.Error("read the signed-in user for the chrome",
			slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
		return Viewer{}, false
	}

	return Viewer{
		UserID:      u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		IsSuperuser: u.IsSuperuser,
	}, true
}
