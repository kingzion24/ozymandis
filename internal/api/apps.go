package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Whoami is what a credential turns out to be.
//
// The first call any client makes, and the one `oz auth login` uses to check a
// token before writing it to disk — a CLI that stores an invalid credential and
// fails on the next command has reported the error in the wrong place.
type Whoami struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name,omitempty"`
	Role     string `json:"role"`
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	role, _ := s.roleOf(r) // proven by the gate this handler is behind
	writeJSON(w, s.log, http.StatusOK, Whoami{
		TeamID: owner.ID, TeamName: owner.DisplayName, Role: string(role),
	})
}

// App is one workload, as JSON.
//
// A type of this package's own rather than app.App marshalled directly. The
// service struct carries fields that have no business on the wire — and, more
// importantly, marshalling it directly would mean any field added there
// appears here without anybody deciding it should. That is how a secret ends up
// in an API response.
type App struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int32  `json:"replicas"`
	Port      int32  `json:"port"`
	Source    string `json:"source"`
	Internal  bool   `json:"internal"`
	Command   string `json:"command,omitempty"`

	RepoURL    string `json:"repo_url,omitempty"`
	RepoBranch string `json:"repo_branch,omitempty"`
	RepoSubdir string `json:"repo_subdir,omitempty"`

	// DeployKey is the PUBLIC half, readable because it is public and because
	// the alternative is minting a new pair every time somebody needs to see
	// which key a repository should trust — which revokes the one already
	// working there.
	DeployKey string `json:"deploy_key,omitempty"`

	HealthPath string `json:"health_path,omitempty"`
	Liveness   bool   `json:"liveness,omitempty"`

	// Host is the platform-issued hostname, and TLS whether it is served over
	// one. Both come from the install's configuration rather than the app row,
	// so a client building a URL has what it needs without knowing the
	// install's hostname policy.
	Host string `json:"host,omitempty"`
	TLS  bool   `json:"tls,omitempty"`

	Status *Status `json:"status,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Status is what the cluster observes.
//
// A pointer on App, and absent rather than zero when the cluster did not
// answer. "Not running" and "we could not ask" are different facts, and a
// zero-valued status would report the second as the first — which for a
// health check is the difference between paging somebody and not.
type Status struct {
	Phase     string `json:"phase"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	Available int32  `json:"available"`
	Message   string `json:"message,omitempty"`
}

func appOut(a app.App) App {
	out := App{
		Name:      a.Name,
		Namespace: a.Namespace,
		Image:     a.Image,
		Replicas:  a.Replicas,
		Port:      a.Port,
		Source:    string(a.Source),
		Internal:  a.Internal,
		Command:   a.Command,

		RepoURL:    a.Repo.URL,
		RepoBranch: a.Repo.Branch,
		RepoSubdir: a.Repo.Subdir,
		DeployKey:  a.DeployKeyPublic,

		HealthPath: a.HealthPath,
		Liveness:   a.Liveness,

		Host: a.Host,
		TLS:  a.TLS,

		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
	if a.StatusKnown {
		out.Status = &Status{
			Phase:     string(a.Status.Phase),
			Desired:   a.Status.Desired,
			Ready:     a.Status.Ready,
			Available: a.Status.Available,
			Message:   a.Status.Message,
		}
	}
	return out
}

func (s *Server) appList(w http.ResponseWriter, r *http.Request) {
	apps, err := s.apps.List(r.Context(), ownerOf(r).ID)
	if err != nil {
		writeServiceError(w, s.log, "list apps", err)
		return
	}
	out := make([]App, 0, len(apps))
	for _, a := range apps {
		out = append(out, appOut(a))
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) appGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, appOut(a))
}

// appStatus is the cheap endpoint a watch loop hits.
//
// Separate from appGet because `oz deploy --watch` polls it every second or so
// and has no use for the repository, the command line, or the timestamps.
func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "app status", err)
		return
	}
	if !a.StatusKnown {
		// 503 rather than an empty status. The caller asked the cluster a
		// question and the cluster did not answer; reporting that as "zero
		// replicas ready" would have a watch loop conclude the app is down.
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"the cluster did not answer; this is not a statement about the app")
		return
	}
	writeJSON(w, s.log, http.StatusOK, Status{
		Phase:     string(a.Status.Phase),
		Desired:   a.Status.Desired,
		Ready:     a.Status.Ready,
		Available: a.Status.Available,
		Message:   a.Status.Message,
	})
}

// CreateApp is the body of POST /apps.
type CreateApp struct {
	Name     string            `json:"name"`
	Source   string            `json:"source"`
	Image    string            `json:"image,omitempty"`
	Port     int32             `json:"port,omitempty"`
	Replicas int32             `json:"replicas,omitempty"`
	Command  string            `json:"command,omitempty"`
	Env      map[string]string `json:"env,omitempty"`

	RepoURL    string `json:"repo_url,omitempty"`
	RepoBranch string `json:"repo_branch,omitempty"`
	RepoSubdir string `json:"repo_subdir,omitempty"`
}

func (s *Server) appCreate(w http.ResponseWriter, r *http.Request) {
	var in CreateApp
	if err := decodeJSON(r, &in); err != nil {
		writeInvalid(w, "that body is not the JSON this expects: "+err.Error())
		return
	}

	// Validated here, before the service, so that a malformed request comes
	// back as a 400 naming the field rather than as whatever the service makes
	// of it. The validators are the app package's own — a second opinion here
	// would be a second set of rules to keep in agreement.
	if err := orchestrator.ValidateDNSLabel("name", in.Name); err != nil {
		writeInvalid(w, err.Error())
		return
	}
	repo := app.Repo{URL: in.RepoURL, Branch: in.RepoBranch, Subdir: in.RepoSubdir}.Normalise()
	if err := repo.Validate(); err != nil {
		writeInvalid(w, err.Error())
		return
	}
	if in.Command != "" {
		if _, err := app.ParseCommand(in.Command); err != nil {
			writeInvalid(w, err.Error())
			return
		}
	}
	if in.Replicas < 0 {
		writeInvalid(w, "replicas must not be negative")
		return
	}

	a, err := s.apps.Create(r.Context(), ownerOf(r).ID, app.CreateInput{
		Name:     in.Name,
		Source:   app.Source(in.Source),
		Image:    in.Image,
		Port:     in.Port,
		Replicas: in.Replicas,
		Command:  in.Command,
		Env:      in.Env,
		Repo:     repo,
	})
	if err != nil {
		writeServiceError(w, s.log, "create app", err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, appOut(a))
}

func (s *Server) appDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.apps.Delete(r.Context(), ownerOf(r).ID, name); err != nil {
		writeServiceError(w, s.log, "delete app", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Deploy is the body of POST /apps/{name}/deploy. Empty is valid and means
// "redeploy what is configured".
type Deploy struct{}

// appDeploy starts a deployment and returns immediately.
//
// 202, not 200. A repository app builds for minutes; the deployment row exists
// the moment this returns and the caller polls it. Holding the request open
// would put a build behind whatever proxy timeout sits in front of this.
func (s *Server) appDeploy(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	name := chi.URLParam(r, "name")

	if err := s.apps.Redeploy(r.Context(), owner.ID, name); err != nil {
		writeServiceError(w, s.log, "deploy app", err)
		return
	}

	// The deployment just started is the most recent one. Read back rather than
	// returned from Redeploy, which reports only whether it began — changing
	// that signature to suit this caller would push an API concern into the
	// service every other caller shares.
	a, err := s.apps.Get(r.Context(), owner.ID, name)
	if err != nil {
		writeServiceError(w, s.log, "read app after deploy", err)
		return
	}
	deps, err := s.apps.Deployments(r.Context(), owner.ID, a.ID, 1)
	if err != nil || len(deps) == 0 {
		// The deploy did start; failing to read its row back is not a reason to
		// tell the caller otherwise.
		writeJSON(w, s.log, http.StatusAccepted, map[string]any{"app": appOut(a)})
		return
	}
	writeJSON(w, s.log, http.StatusAccepted, map[string]any{
		"app": appOut(a), "deployment": deploymentOut(deps[0]),
	})
}

// Scale is the body of POST /apps/{name}/scale.
type Scale struct {
	Replicas *int32 `json:"replicas"`
}

func (s *Server) appScale(w http.ResponseWriter, r *http.Request) {
	var in Scale
	if err := decodeJSON(r, &in); err != nil {
		writeInvalid(w, "that body is not the JSON this expects: "+err.Error())
		return
	}
	// A pointer, so that {"replicas": 0} — scale to nothing, a real request —
	// is distinguishable from a body that forgot the field.
	if in.Replicas == nil {
		writeInvalid(w, "replicas is required")
		return
	}
	if *in.Replicas < 0 {
		writeInvalid(w, "replicas must not be negative")
		return
	}

	a, err := s.apps.Scale(r.Context(), ownerOf(r).ID, chi.URLParam(r, "name"), *in.Replicas)
	if err != nil {
		writeServiceError(w, s.log, "scale app", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, appOut(a))
}

// Deployment is one deploy, as JSON.
//
// Finished is carried explicitly rather than left for the client to infer from
// FinishedAt being absent. `oz deploy --watch` polls until a deploy ends, and
// "did this finish" is the entire question it is asking — making every client
// re-derive it from a null is how one of them gets it wrong and waits forever.
type Deployment struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Source   string `json:"source,omitempty"`
	Image    string `json:"image,omitempty"`
	Message  string `json:"message,omitempty"`
	Finished bool   `json:"finished"`

	// ReleaseStatus is what the release command did: skipped, succeeded,
	// failed, or unavailable. Empty on a deployment that predates the feature.
	//
	// Surfaced because "did my migrations run" is the question a deploy leaves
	// somebody with, and it is not answerable from Status alone: a deployment
	// can succeed with no release, and one that failed may have failed before
	// the release was reached.
	ReleaseStatus string `json:"release_status,omitempty"`
	ReleaseLog    string `json:"release_log,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func deploymentOut(d app.Deployment) Deployment {
	return Deployment{
		ID:       d.ID.String(),
		Status:   d.Status,
		Source:   d.Source(),
		Image:    d.Image,
		Message:  d.Message,
		Finished: d.FinishedAt != nil,

		ReleaseStatus: d.ReleaseStatus,
		ReleaseLog:    d.ReleaseLog,

		StartedAt:  d.StartedAt,
		FinishedAt: d.FinishedAt,
	}
}

func (s *Server) deploymentList(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	limit := int32(20)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeInvalid(w, "limit must be a number between 1 and 200")
			return
		}
		limit = int32(n)
	}

	deps, err := s.apps.Deployments(r.Context(), ownerOf(r).ID, a.ID, limit)
	if err != nil {
		writeServiceError(w, s.log, "list deployments", err)
		return
	}
	out := make([]Deployment, 0, len(deps))
	for _, d := range deps {
		out = append(out, deploymentOut(d))
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{"deployments": out})
}

// lookup reads the app named in the path, scoped to the caller's team.
//
// Every handler goes through this rather than calling Get directly, so the
// owner scoping is applied in one place. An app belonging to another team comes
// back as app.ErrNotFound from the service — the scoping is in the query — and
// is reported as a 404 rather than a 403, so the endpoint cannot be used to
// find out which names another team has taken.
func (s *Server) lookup(r *http.Request) (app.App, error) {
	name := chi.URLParam(r, "name")
	if name == "" {
		return app.App{}, app.ErrNotFound
	}
	a, err := s.apps.Get(r.Context(), ownerOf(r).ID, name)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return app.App{}, app.ErrNotFound
		}
		return app.App{}, err
	}
	return a, nil
}

// DeployKeyOut is the body of POST /apps/{name}/deploy-key.
//
// Only the public half, because only the public half ever leaves: the private
// one is sealed on the way in and unsealed only by a build about to clone with
// it. There is no endpoint that returns it, and this is not an oversight.
type DeployKeyOut struct {
	Public string `json:"public"`
}

// deployKeyGenerate mints a key pair for cloning a private repository and
// returns the public half to add to the repository host.
//
// The dashboard has had this since deploy-on-push existed; the API had not,
// which left a gap you could fall into without noticing: this API can create an
// app from a private repository, but could not give it the credential to clone
// one. Every build then failed at the clone with exit 128, and the fix lived on
// a page a script cannot reach.
//
// POST rather than GET because it is not a read. Calling it twice mints two
// pairs and the second replaces the first — which is how a leaked key is
// revoked, and why the response says so rather than being a bare string.
func (s *Server) deployKeyGenerate(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	key, err := s.pushes.GenerateDeployKey(r.Context(), ownerOf(r).ID, name)
	if err != nil {
		writeServiceError(w, s.log, "generate deploy key", err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, DeployKeyOut{Public: key.Public})
}
