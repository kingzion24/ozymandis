package api

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/identity"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Apps is the workload lifecycle this API drives.
//
// Declared here, at the consumer, for the reason web.Apps is declared there:
// the app package stays free of knowledge about who calls it, and this package
// names only what it actually uses. The overlap with web.Apps is real and
// deliberate — two callers wanting the same operations is not duplication, and
// merging them into one shared interface would mean every method either surface
// adds becomes a method the other has to care about.
type Apps interface {
	List(ctx context.Context, ownerID string) ([]app.App, error)
	Get(ctx context.Context, ownerID, name string) (app.App, error)
	Create(ctx context.Context, ownerID string, in app.CreateInput) (app.App, error)
	Scale(ctx context.Context, ownerID, name string, replicas int32) (app.App, error)
	Redeploy(ctx context.Context, ownerID, name string) error
	Delete(ctx context.Context, ownerID, name string) error
	Deployments(ctx context.Context, ownerID string, appID uuid.UUID, limit int32) ([]app.Deployment, error)

	SetVariable(ctx context.Context, ownerID, appName string, in app.VariableInput) error
	DeleteVariable(ctx context.Context, ownerID, appName, key string) error

	SetHealth(ctx context.Context, ownerID, name, healthPath string, liveness bool) error
	SetService(ctx context.Context, ownerID, name string, port int32, internal bool) error
	SetCommand(ctx context.Context, ownerID, name, command string) error
	SetReleaseCommand(ctx context.Context, ownerID, name, command string) error
}

// Nets is the routing surface, optional in the same way Logs is.
//
// RemoveDomain is here even though the CONVERGE never calls it. The two are
// different questions: a config converge is additive because a file that stops
// mentioning a hostname has not asked for it to be deleted — but somebody who
// actually wants it gone needs a verb, and without one the converge's "left
// alone and reported" would be a report with no corresponding action, which is
// how a person ends up editing the database.
//
// So removal exists and is explicit: its own endpoint, its own command, never
// a side effect of deploying.
type Nets interface {
	Networking(ctx context.Context, ownerID, name string) (app.Networking, error)
	AddDomain(ctx context.Context, ownerID, name, host string) error
	RemoveDomain(ctx context.Context, ownerID, name string, id uuid.UUID) error
}

// Pushes is deploy-on-push, optional for the same reason it is in the
// dashboard: the deploy key is a credential, and an install with no secret key
// declines to hold one rather than storing it readable. Nil leaves the endpoint
// off the router instead of mounted and failing.
type Pushes interface {
	GenerateDeployKey(ctx context.Context, ownerID, name string) (app.DeployKey, error)
}

// Logs is the log surface, optional in the same way the dashboard's is.
type Logs interface {
	Logs(ctx context.Context, ownerID, appName string, req app.LogRequest) (app.Logs, error)
	LogStream(
		ctx context.Context, ownerID, appName string, req app.LogRequest,
	) (iter.Seq2[orchestrator.LogLine, error], error)
}

// Roles answers what the caller may do.
//
// A seam rather than a direct dependency on account.Service, so an install with
// no accounts — resolved by a shared token or by a wrapping application's own
// provider — simply passes nil and every authenticated caller is the owner.
// That is not a permissive default; it is the literal truth of such an install,
// and it is the same reading web.roleOf takes.
type Roles interface {
	// RoleForRequest returns the role the request's own credential carries in
	// the team it is acting as.
	//
	// It takes the request rather than an id because the credential is what
	// proves the role, and a lookup keyed on anything the request merely
	// asserts about itself would be a permission the caller can grant
	// themselves.
	RoleForRequest(ctx context.Context, r *http.Request, ownerID string) (account.Role, error)
}

// Options configures the API server.
type Options struct {
	Identity identity.Provider
	Apps     Apps

	// Roles is optional. Nil means this install has no roles to read, and every
	// authenticated caller acts as the owner.
	Roles Roles

	// Logs is optional; nil leaves the log endpoints off the router rather than
	// mounted and failing.
	Logs Logs

	// Exec is optional. Nil, or an orchestrator that cannot attach, leaves the
	// console endpoint off the router rather than mounted and failing — exec
	// needs the pods/exec subresource specifically, which an install may
	// reasonably not grant.
	Exec Exec

	// Nets is optional. Nil leaves an install with no custom-domain support,
	// where the convergence reports domain changes as skipped rather than
	// failing on a surface that is not there.
	Nets Nets

	// Pushes is optional, and nil on any install without a secret key to seal
	// a deploy key with.
	Pushes Pushes

	Logger *slog.Logger
}

// Server serves /api/v1.
type Server struct {
	ident  identity.Provider
	apps   Apps
	roles  Roles
	logs   Logs
	nets   Nets
	exec   Exec
	pushes Pushes
	log    *slog.Logger
}

// New builds the API server.
func New(opts Options) (*Server, error) {
	if opts.Identity == nil {
		return nil, errors.New("api: an identity provider is required")
	}
	if opts.Apps == nil {
		return nil, errors.New("api: an apps service is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		ident:  opts.Identity,
		apps:   opts.Apps,
		roles:  opts.Roles,
		logs:   opts.Logs,
		nets:   opts.Nets,
		exec:   opts.Exec,
		pushes: opts.Pushes,
		log:    log,
	}, nil
}

// Handler returns the router, mounted at /api/v1.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		// Authentication first, and with no redirect anywhere in it. The
		// dashboard's signInRedirect turns an unauthenticated request into a
		// 302 to an HTML form, which is right for a browser and is, for a CLI,
		// a 200 carrying a login page instead of the data it asked for.
		r.Use(s.authenticate)

		// Reads.
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(account.RoleMember))

			r.Get("/whoami", s.whoami)
			r.Get("/apps", s.appList)
			r.Get("/apps/{name}", s.appGet)
			r.Get("/apps/{name}/status", s.appStatus)
			r.Get("/apps/{name}/deployments", s.deploymentList)
			r.Get("/apps/{name}/secrets", s.secretList)
			r.Get("/apps/{name}/config", s.configGet)

			if s.nets != nil {
				r.Get("/apps/{name}/domains", s.domainList)
			}

			if s.logs != nil {
				r.Get("/apps/{name}/logs", s.appLogs)
			}
		})

		// Writes. Admin, matching the dashboard exactly: the same actions are
		// behind the same role there, and an API that let a member do what the
		// dashboard refuses them would be a way around the gate rather than a
		// second way to reach it.
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(account.RoleAdmin))

			r.Post("/apps", s.appCreate)
			r.Delete("/apps/{name}", s.appDelete)
			r.Post("/apps/{name}/deploy", s.appDeploy)
			r.Post("/apps/{name}/scale", s.appScale)
			r.Put("/apps/{name}/config", s.configPut)
			r.Put("/apps/{name}/secrets", s.secretSet)
			r.Delete("/apps/{name}/secrets/{key}", s.secretDelete)

			// The most sensitive endpoint here: a shell in a production
			// container reads every secret the app holds. Admin, the same role
			// that can delete the app, and off the router entirely when this
			// install cannot attach.
			if s.exec != nil && s.exec.CanExec() {
				r.Get("/apps/{name}/exec", s.appExec)
			}

			// Same rule as the dashboard's: gated on a secret key existing,
			// because minting a deploy key this install cannot seal would
			// mean storing a private key readable.
			if s.pushes != nil {
				r.Post("/apps/{name}/deploy-key", s.deployKeyGenerate)
			}

			// Off the router entirely without a networking surface, rather
			// than mounted and dispatching on a nil interface.
			if s.nets != nil {
				r.Post("/apps/{name}/domains", s.domainAdd)
				r.Delete("/apps/{name}/domains/{id}", s.domainRemove)
			}
		})
	})

	// Anything under /api/ that did not match is a JSON 404, not the router's
	// plain-text default. A client that parses every response as JSON should
	// not have to special-case the one that says "404 page not found".
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, CodeInvalid,
			"that method is not allowed on this endpoint")
	})

	return r
}

// authenticate resolves the owner, failing as JSON.
//
// Its own middleware rather than identity.Middleware, which writes
// `http.Error(w, "unauthorized", 401)` — a text/plain body. Everything else
// here is JSON, and one endpoint that is not is one a client parses wrongly
// precisely when something has already gone wrong.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.ident.Resolve(r.Context(), r)
		if err != nil || !owner.Valid() {
			writeError(w, http.StatusUnauthorized, CodeUnauthenticated,
				"provide a token: Authorization: Bearer oz_…")
			return
		}
		next.ServeHTTP(w, r.WithContext(identity.NewContext(r.Context(), owner)))
	})
}

// requireRole refuses a request whose credential does not carry at least min.
//
// Mounted with r.Use on a route group and never called from a handler, for the
// reason web.requireRole gives: a check each handler has to remember is one
// some handler will forget, and the forgotten one is the security bug.
func (s *Server) requireRole(min account.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := s.roleOf(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, CodeUnauthenticated,
					"that credential is no longer valid")
				return
			}
			if !role.AtLeast(min) {
				// Said plainly, and 403 rather than 404. The caller is proven
				// to be in this team, so what they may do in it is not a secret
				// being kept from them — unlike the existence of another team's
				// app, which is why that case is a 404.
				writeError(w, http.StatusForbidden, CodeForbidden,
					"your role in this team does not allow that")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// roleOf reports the role the request's own credential carries.
func (s *Server) roleOf(r *http.Request) (account.Role, bool) {
	owner, ok := identity.FromContext(r.Context())
	if !ok {
		// Only reachable if a gate is mounted outside the authenticated group.
		// Refusing is the safe reading of a wiring mistake.
		return "", false
	}

	// An install with no account service has no roles to read: its owner comes
	// from an injected provider — a shared token, or a wrapping application's
	// own sessions — and that principal is the whole owner. Refusing here would
	// lock every such install out of its own API.
	if s.roles == nil {
		return account.RoleOwner, true
	}

	role, err := s.roles.RoleForRequest(r.Context(), r, owner.ID)
	if err != nil {
		if !errors.Is(err, account.ErrTokenNotValid) &&
			!errors.Is(err, account.ErrSessionInvalid) &&
			!errors.Is(err, account.ErrNotAMember) {
			s.log.Error("resolve role for an api gate", slog.String("error", err.Error()))
		}
		return "", false
	}
	return role, true
}

// ownerOf is the team the request acts as.
func ownerOf(r *http.Request) identity.Owner {
	return identity.MustFromContext(r.Context())
}
