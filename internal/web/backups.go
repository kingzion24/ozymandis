package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/backup"
	"github.com/kingzion24/ozymandis/internal/identity"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Backups is the dashboard's view of the backup surface.
//
// Its own interface rather than more methods on Apps, and left off the router
// entirely when nil — the same treatment the add-node and registry surfaces
// get. An install whose orchestrator cannot run Jobs has no use for these
// pages, and offering them to be refused is worse than not offering them.
type Backups interface {
	Backups(ctx context.Context, ownerID, name string) (app.Backups, error)
	SetBackupPolicy(ctx context.Context, ownerID, name string, p backup.Policy) error
	DisableBackups(ctx context.Context, ownerID, name string) error
	BackupNow(ctx context.Context, ownerID, name, target string) (orchestrator.TaskResult, error)
	BackupSnapshots(ctx context.Context, ownerID, name string) ([]backup.Snapshot, error)
	RestoreBackup(ctx context.Context, ownerID, name, target, snapshot string) (orchestrator.TaskResult, error)

	BackupDestination(ctx context.Context, ownerID string) (backup.Destination, bool, error)
	SetBackupDestination(ctx context.Context, ownerID string, d backup.Destination) error
	ClearBackupDestination(ctx context.Context, ownerID string) error
}

// BackupsData is what the per-app backups page renders.
type BackupsData struct {
	App  string
	Data app.Backups

	// Snapshots is populated only after somebody asks for them, because
	// listing runs a Job against the repository. A page that listed on every
	// render would start a container each time an app was opened.
	Snapshots []backup.Snapshot
	Listed    bool

	// Log is the output of a backup or restore that was just run, shown
	// because it is the only place the reason for a failure appears.
	Log string

	Notice string
	Error  string
}

// DestinationData is what the install-wide destination page renders.
type DestinationData struct {
	Destination backup.Destination
	Configured  bool

	// KeyRequired reports that credentials cannot be sealed, so the form
	// explains why rather than accepting values it would have to store
	// readable.
	KeyRequired bool

	Notice string
	Error  string
}

// appBackups renders an app's backup configuration.
func (s *Server) appBackups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	data, err := s.backups.Backups(ctx, owner.ID, name)
	if err != nil {
		s.backupsFailed(w, r, name, err)
		return
	}

	s.renderWithCrumb(w, r, AppBackups(BackupsData{
		App:    name,
		Data:   data,
		Notice: r.URL.Query().Get("notice"),
		Error:  r.URL.Query().Get("error"),
	}), name)
}

// backupPolicySet stores a policy and converges the schedules behind it.
func (s *Server) backupPolicySet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	p := backup.Policy{
		Enabled:     r.FormValue("enabled") == "on",
		Schedule:    strings.TrimSpace(r.FormValue("schedule")),
		KeepDaily:   formInt(r.FormValue("keep_daily")),
		KeepWeekly:  formInt(r.FormValue("keep_weekly")),
		KeepMonthly: formInt(r.FormValue("keep_monthly")),
	}
	if p.Schedule == "" {
		p.Schedule = backup.DefaultSchedule
	}

	if err := s.backups.SetBackupPolicy(ctx, owner.ID, name, p); err != nil {
		s.backupsFailed(w, r, name, err)
		return
	}
	s.redirectWith(w, r, "/apps/"+name+"/backups", "notice",
		"Backups scheduled. Take one now to check the destination works — "+
			"a schedule nobody has seen succeed is not yet a backup.")
}

// backupDisable stops backing an app up, leaving what has been written alone.
func (s *Server) backupDisable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	if err := s.backups.DisableBackups(ctx, owner.ID, name); err != nil {
		s.backupsFailed(w, r, name, err)
		return
	}
	s.redirectWith(w, r, "/apps/"+name+"/backups", "notice",
		"Backups switched off. The snapshots already taken are untouched.")
}

// backupNow runs a backup and shows its log.
//
// Synchronous, and deliberately so. This is the button somebody presses to find
// out whether their destination works, and an answer that arrives later
// somewhere else is not an answer to that question.
func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	target := r.FormValue("target")

	res, err := s.backups.BackupNow(ctx, owner.ID, name, target)
	data, dataErr := s.backups.Backups(ctx, owner.ID, name)
	if dataErr != nil {
		s.backupsFailed(w, r, name, dataErr)
		return
	}

	out := BackupsData{App: name, Data: data, Log: res.Output}
	switch {
	case err != nil:
		out.Error = err.Error()
	case !res.Succeeded:
		out.Error = "The backup did not finish. The log below says why."
	default:
		out.Notice = "Backup taken."
	}
	s.renderWithCrumb(w, r, AppBackups(out), name)
}

// backupSnapshots lists what the repository holds.
func (s *Server) backupSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	data, err := s.backups.Backups(ctx, owner.ID, name)
	if err != nil {
		s.backupsFailed(w, r, name, err)
		return
	}

	out := BackupsData{App: name, Data: data, Listed: true}
	snaps, err := s.backups.BackupSnapshots(ctx, owner.ID, name)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Snapshots = snaps
		if len(snaps) == 0 {
			out.Notice = "The repository is reachable and holds no snapshots yet."
		}
	}
	s.renderWithCrumb(w, r, AppBackups(out), name)
}

// backupRestore writes a snapshot back over the app's live data.
func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	res, err := s.backups.RestoreBackup(ctx, owner.ID, name,
		r.FormValue("target"), r.FormValue("snapshot"))

	data, dataErr := s.backups.Backups(ctx, owner.ID, name)
	if dataErr != nil {
		s.backupsFailed(w, r, name, dataErr)
		return
	}

	out := BackupsData{App: name, Data: data, Log: res.Output}
	switch {
	case err != nil:
		out.Error = err.Error()
	case !res.Succeeded:
		// Said plainly, because a half-finished restore is the state where
		// somebody most needs to know not to assume it worked.
		out.Error = "The restore did not finish. The data may be partly replaced — " +
			"read the log before doing anything else."
	default:
		out.Notice = "Restored."
	}
	s.renderWithCrumb(w, r, AppBackups(out), name)
}

// backupDestination renders the install-wide storage settings.
func (s *Server) backupDestination(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	d, ok, err := s.backups.BackupDestination(ctx, owner.ID)
	if err != nil {
		s.render(w, r, BackupDestination(DestinationData{Error: err.Error()}))
		return
	}
	s.render(w, r, BackupDestination(DestinationData{
		Destination: d,
		Configured:  ok,
		Notice:      r.URL.Query().Get("notice"),
		Error:       r.URL.Query().Get("error"),
	}))
}

// backupDestinationSet stores where backups go.
func (s *Server) backupDestinationSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	d := backup.Destination{
		Endpoint:        strings.TrimSpace(r.FormValue("endpoint")),
		Bucket:          strings.TrimSpace(r.FormValue("bucket")),
		Prefix:          strings.TrimSpace(r.FormValue("prefix")),
		Region:          strings.TrimSpace(r.FormValue("region")),
		AccessKeyID:     strings.TrimSpace(r.FormValue("access_key_id")),
		SecretAccessKey: strings.TrimSpace(r.FormValue("secret_access_key")),
		RepoPassword:    strings.TrimSpace(r.FormValue("repo_password")),
	}

	if err := s.backups.SetBackupDestination(ctx, owner.ID, d); err != nil {
		s.redirectWith(w, r, "/settings/backups", "error", err.Error())
		return
	}
	s.redirectWith(w, r, "/settings/backups", "notice",
		"Destination saved. Back an app up now to confirm it works — the "+
			"repository password cannot be changed once the first snapshot exists.")
}

// backupDestinationClear removes the destination and the schedules that used it.
func (s *Server) backupDestinationClear(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	if err := s.backups.ClearBackupDestination(ctx, owner.ID); err != nil {
		s.redirectWith(w, r, "/settings/backups", "error", err.Error())
		return
	}
	s.redirectWith(w, r, "/settings/backups", "notice",
		"Destination removed, and every backup schedule with it. What has "+
			"already been written to the bucket is untouched.")
}

// formInt reads a count from a form, treating anything unparseable as zero.
//
// Zero is a legal retention count for one band — the policy only has to keep
// something overall, which backup.Policy.Validate enforces. So a bad value
// becoming zero cannot produce a policy that deletes everything; it produces
// one that is refused if every band ends up zero.
func formInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// backupsFailed re-renders the backups page with the reason it refused.
//
// Its own helper rather than appActionFailed, because backups are their own
// page rather than a tab on the app detail — the same way logs are. Reusing the
// tab helper would answer a backup question on a page with no backup panel.
func (s *Server) backupsFailed(
	w http.ResponseWriter, r *http.Request, name string, cause error,
) {
	status := http.StatusUnprocessableEntity
	if errors.Is(cause, app.ErrNotFound) {
		status = http.StatusNotFound
	}

	// Best effort: an error reading the configuration is exactly the case
	// where the page still has to render, so it can say what went wrong.
	data, err := s.backups.Backups(r.Context(), identity.MustFromContext(r.Context()).ID, name)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}

	w.WriteHeader(status)
	s.renderWithCrumb(w, r, AppBackups(BackupsData{
		App: name, Data: data, Error: cause.Error(),
	}), name)
}

// redirectWith redirects carrying a message the destination page reads back out
// of the query string.
//
// Used rather than re-rendering, so a refresh after a save does not repost the
// form — these actions write to a cluster, and a repost would run a second one.
func (s *Server) redirectWith(
	w http.ResponseWriter, r *http.Request, path, key, message string,
) {
	http.Redirect(w, r, path+"?"+url.Values{key: {message}}.Encode(),
		http.StatusSeeOther)
}
