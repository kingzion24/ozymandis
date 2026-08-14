package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/kingzion24/ozymandis/internal/account"
)

// SessionCookie is the name of the cookie carrying a session token.
//
// It matches account.DefaultCookieName because the session provider resolves
// requests by reading a cookie of that name: a mismatch would write a cookie
// nothing reads.
const SessionCookie = "ozymandis_session"

// requestIsTLS reports whether the request reached us over TLS.
//
// X-Forwarded-Proto is honoured because the common deployment terminates TLS
// at a reverse proxy and forwards plain HTTP. Reading only r.TLS would drop
// the Secure flag on exactly the installs that have a certificate.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie writes the session cookie.
//
// Secure is conditional on purpose. Setting it unconditionally makes the
// browser drop the cookie on a plain-HTTP install, so sign-in appears to
// succeed and then does nothing, with nothing on screen explaining why.
//
// SameSite=Lax is the CSRF defence, which is why no form in this codebase
// carries a token: a cross-site POST does not get the cookie attached.
func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearSessionCookie expires the session cookie.
//
// The attributes repeat those of setSessionCookie because a browser only
// replaces a cookie whose name, path and domain match: clearing with a
// different Path would leave the live cookie in place and sign-out would do
// nothing.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: requestIsTLS(r), MaxAge: -1,
	})
}

// signInFloor is the minimum time a sign-in POST takes.
//
// Issuing a link for a registered address does an extra INSERT that an
// unregistered address does not, which is measurable — a 3.5x difference was
// demonstrated against this code. Padding to a floor removes the signal
// without pretending the two paths do equal work.
//
// The floor is above the slow path, so the pad is always a wait rather than a
// race the fast path can lose.
const signInFloor = 250 * time.Millisecond

// Attempts allowed in a window, per address and per client.
//
// The floor already makes a single attempt cost 250ms; these are what stop an
// enumeration run from simply being made in parallel. The per-address limit is
// the tighter one because it is the one an enumerator cannot avoid — every
// address they want to test needs its own attempt.
const (
	signInAttemptsPerAddress = 5
	signInAttemptsPerIP      = 20
	signInWindow             = 15 * time.Minute
)

// withFloor runs fn and does not return until d has elapsed.
func withFloor(d time.Duration, fn func()) {
	start := time.Now()
	fn()
	if rest := d - time.Since(start); rest > 0 {
		time.Sleep(rest)
	}
}

// signInPage serves the form. It sits outside the authenticated group: someone
// with no session is exactly who needs it.
func (s *Server) signInPage(w http.ResponseWriter, r *http.Request) {
	s.renderSignedOut(w, r, http.StatusOK, "Sign in", SignIn(SignInData{}))
}

// signInRequest checks a username and password and opens a session.
//
// Every way of failing is answered identically — same page, same status, and
// through the floor, near enough the same time. A wrong password and a name
// that belongs to nobody must not be distinguishable, or the form becomes a way
// to discover who has an account on this install.
func (s *Server) signInRequest(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username == "" || password == "" {
		// Answered without the floor on purpose: an empty field is a mistake the
		// visitor can already see, not a fact about who has an account.
		s.renderSignedOut(w, r, http.StatusUnprocessableEntity, "Sign in", SignIn(SignInData{
			Username: username,
			Error:    "Enter your username and password.",
		}))
		return
	}

	// Both counters are asked, not short-circuited, so a name already over its
	// limit still counts against the client that keeps trying it. Lowercased
	// for the counter because the lookup is case-insensitive: without it,
	// alternating the capitalisation of one name buys an unlimited budget.
	userOK := s.signInLimit.allow("user:"+strings.ToLower(username), signInAttemptsPerAddress)
	clientOK := s.signInLimit.allow("client:"+clientIP(r), signInAttemptsPerIP)
	if !userOK || !clientOK {
		// Refusal depends on the attempt count and never on whether the name
		// exists, so saying so plainly leaks nothing.
		s.renderSignedOut(w, r, http.StatusTooManyRequests, "Sign in", SignIn(SignInData{
			Username: username,
			Error:    "Too many sign-in attempts. Try again in a few minutes.",
		}))
		return
	}

	var (
		user account.User
		err  error
	)
	withFloor(signInFloor, func() {
		user, err = s.accounts.Authenticate(r.Context(), username, password)
	})
	if err != nil {
		if !errors.Is(err, account.ErrBadCredentials) {
			s.log.Error("authenticate", slog.String("error", err.Error()))
		}
		s.renderSignedOut(w, r, http.StatusUnauthorized, "Sign in", SignIn(SignInData{
			Username: username,
			Error:    "Incorrect username or password.",
		}))
		return
	}

	s.openSession(w, r, user)
}

// openSession finds a team for the person and issues the cookie.
//
// Shared by sign-in and by a password change that re-establishes the session,
// so that "which team does this person act as" is decided in one place.
func (s *Server) openSession(w http.ResponseWriter, r *http.Request, user account.User) {
	ctx := r.Context()

	team, err := s.activeTeam(ctx, user)
	if err != nil {
		s.log.Error("find a team to sign in to",
			slog.String("user", user.ID.String()), slog.String("error", err.Error()))
		s.renderSignedOut(w, r, http.StatusInternalServerError, "Sign in", SignIn(SignInData{
			Error: "Something went wrong signing you in. Try again in a moment.",
		}))
		return
	}

	raw, err := s.accounts.CreateSession(
		ctx, user.ID, team, r.UserAgent(), clientIP(r), s.sessionTTL)
	if err != nil {
		s.log.Error("create session",
			slog.String("user", user.ID.String()), slog.String("error", err.Error()))
		s.renderSignedOut(w, r, http.StatusInternalServerError, "Sign in", SignIn(SignInData{
			Error: "Something went wrong signing you in. Try again in a moment.",
		}))
		return
	}

	setSessionCookie(w, r, raw, s.sessionTTL)
	s.log.Info("signed in",
		slog.String("user", user.ID.String()),
		slog.String("username", user.Username),
		slog.String("team", team))

	// See other rather than a temporary redirect, so a browser repeating this
	// from history does not re-post the credentials.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signOut ends the session this browser holds.
//
// It sits outside the authenticated group on purpose. A session can already be
// unusable before anyone clicks sign out — revoked from another browser, or
// ended by the person being removed from the team — and a sign-out that first
// demanded a working session would answer 401 and leave that browser holding a
// cookie it has no way to drop.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	raw := sessionToken(r)
	if raw != "" {
		if err := s.accounts.RevokeSession(r.Context(), raw); err != nil {
			s.log.Error("revoke session", slog.String("error", err.Error()))
			// The cookie is left alone. Clearing it here would make the browser
			// forget a session that is still live in the database, so the person
			// would believe they had signed out and have no handle left to try
			// again with.
			http.Error(w, "could not sign you out", http.StatusInternalServerError)
			return
		}
	}
	s.endSession(w, r)
}

// signOutEverywhere ends every session the person holds, on every device.
//
// This is the answer to a cookie that has been copied off a machine. Without it
// a leaked session runs until it expires and its owner can do nothing at all
// about it.
func (s *Server) signOutEverywhere(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if raw := sessionToken(r); raw != "" {
		// Whose sessions to end comes from resolving the presented cookie, never
		// from the request body. A user id read from a form would be a way to sign
		// anybody out by knowing their id.
		sess, err := s.accounts.ResolveSession(ctx, raw)
		switch {
		case errors.Is(err, account.ErrSessionInvalid):
			// The cookie is already dead. There is nothing to revoke, and saying
			// so would only tell the holder of a guessed token that they missed.
		case err != nil:
			s.log.Error("resolve session", slog.String("error", err.Error()))
			http.Error(w, "could not sign you out", http.StatusInternalServerError)
			return
		default:
			if err := s.accounts.RevokeAllSessions(ctx, sess.UserID); err != nil {
				s.log.Error("revoke every session",
					slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
				// Reported rather than swallowed: this is the request someone makes
				// when they believe a session has been stolen, and a silent failure
				// leaves them sure the thief is locked out when they are not.
				http.Error(w, "could not sign you out everywhere", http.StatusInternalServerError)
				return
			}
			s.log.Info("signed out everywhere", slog.String("user", sess.UserID.String()))
		}
	}
	s.endSession(w, r)
}

// endSession drops the cookie and sends the browser to the only page it can
// still use.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	// See other rather than a temporary redirect: the POST has been carried out,
	// and a browser that repeated it from history would post to a route that no
	// longer has a session to end.
	http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
}

// sessionToken returns the raw token the request presented, or "".
//
// A missing cookie is not an error here: sign-out has to work for a browser
// whose cookie has already gone.
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// activeTeam picks the team a new session acts as.
//
// Someone with no team at all is the case the bootstrap exists for: on an
// install that has just switched accounts on they are the first person in, and
// the team the install was already running as is theirs to inherit. Without it
// they would sign in successfully and resolve to no owner, which every page
// reads as not being signed in at all.
func (s *Server) activeTeam(ctx context.Context, user account.User) (string, error) {
	teams, err := s.accounts.TeamsFor(ctx, user.ID)
	if err != nil {
		return "", err
	}
	if len(teams) == 0 {
		if err := s.accounts.BootstrapOwner(
			ctx, s.bootstrapID, s.bootstrapName, user); err != nil {
			return "", err
		}
		if teams, err = s.accounts.TeamsFor(ctx, user.ID); err != nil {
			return "", err
		}
		if len(teams) == 0 {
			return "", errors.New("web: the bootstrap left the person with no team")
		}
	}
	// Whichever comes first by team name. A session opens onto one team and the
	// switcher moves it; there is nothing here to prefer, and preferring the
	// most recently used would need a column that does not exist.
	return teams[0].TeamID, nil
}

func signInBody(link string, ttl time.Duration) string {
	return "Someone asked to sign in to Ozymandis with this address.\n\n" +
		"Follow this link to sign in:\n\n" +
		link + "\n\n" +
		"The link works once and expires in " + ttl.String() + ".\n" +
		"If it was not you, nothing has happened and you can ignore this message.\n"
}

// parseEmail accepts what a person types into the form and returns the address.
//
// mail.ParseAddress also accepts `Name <addr>`; only the address is kept, so a
// display name cannot smuggle anything into what is stored or mailed.
func parseEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errEmailRequired
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address == "" {
		return "", errEmailMalformed
	}
	return addr.Address, nil
}

var (
	errEmailRequired  = signInError("Enter the email address you sign in with.")
	errEmailMalformed = signInError("That does not look like an email address.")
)

// signInError is a message written for the person reading the page rather than
// for a log.
type signInError string

func (e signInError) Error() string { return string(e) }

// clientIP is the address the rate limiter counts against.
//
// Best effort: behind a proxy chi's RealIP has already replaced RemoteAddr from
// X-Forwarded-For, which a direct caller can also set. That is why the
// per-address limit exists and is the tighter of the two — it is the one an
// enumerator cannot dodge by rotating a header.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// attemptLimiter counts attempts per key over a sliding window.
//
// In memory, and so per process: a restart forgives everyone, and two replicas
// each keep their own count. That is the honest trade for having no dependency
// here — the limit is a brake on enumeration, not a security boundary, and the
// boundary is that the response says nothing either way.
type attemptLimiter struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string][]time.Time
	swept  time.Time
}

// maxTrackedKeys bounds the map between sweeps. Keys are attacker-supplied
// addresses, so an unbounded map is a way to spend the process's memory.
const maxTrackedKeys = 50_000

func newAttemptLimiter(window time.Duration) *attemptLimiter {
	return &attemptLimiter{
		window: window,
		hits:   make(map[string][]time.Time),
		swept:  time.Now(),
	}
}

// allow records an attempt against key and reports whether it is within limit.
func (l *attemptLimiter) allow(key string, limit int) bool {
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.swept) >= l.window || len(l.hits) > maxTrackedKeys {
		for k, ts := range l.hits {
			if kept := recent(ts, cutoff); len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.swept = now
	}

	ts := recent(l.hits[key], cutoff)
	if len(ts) >= limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}

// recent drops the attempts that have fallen out of the window. The slice is
// appended to in order, so the survivors are its tail.
func recent(ts []time.Time, cutoff time.Time) []time.Time {
	for i, t := range ts {
		if t.After(cutoff) {
			return ts[i:]
		}
	}
	return nil
}

// acceptInvitation spends an invitation, or first establishes who is holding it.
//
// The token is not proof of identity. Acceptance is bound to the address the
// invitation names, so a visitor who is not signed in is sent a sign-in link to
// that address rather than let in — which makes a forwarded or intercepted
// open-redirect surface, and the benefit — landing back on the page you asked
// for — is small next to getting that wrong.
// Only GET is redirected. A person navigating needs somewhere to go; a form
// post or an API call with a dead session deserves a plain 401, and turning
// that into a 303 to an HTML page is how a caller ends up parsing a sign-in
// form as though it were a result.
func (s *Server) signInRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := s.accounts.ResolveSession(r.Context(), sessionToken(r)); err != nil {
			http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bootstrapAccount creates the install's configured owner, once.
//
// The sign-in form must not fill the user table, which is why an unknown
// address is silently ignored everywhere else. That rule leaves a fresh
// install unenterable: no user means no link, and no link means no user. This
// is the single exception — one address, named in configuration, and only
// while it has no account.
//
// Failures are swallowed to the log. The caller renders the same page whatever
// happened here, and an error that changed the response would say whether the
// address is the configured one.
