package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kingzion24/ozymandis/internal/backup"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Backups is an app's backup configuration and what it has produced.
type Backups struct {
	// Configured reports that the install has somewhere to write to. Without
	// it every other field is empty and the page explains why rather than
	// offering a switch that could not do anything.
	Configured bool

	// Available reports that this orchestrator can run tasks at all.
	Available bool

	// Because explains an unavailable state, in words for the person reading
	// the page rather than an error for a log.
	Because string

	Policy    backup.Policy
	HasPolicy bool

	// Targets are what would be copied: a database, or one entry per volume.
	Targets []backup.Target

	// Runs is what the schedule has done lately, most recent first. Read from
	// the cluster, so an empty list on a configured app means it has not run
	// yet rather than that nothing was recorded.
	Runs []orchestrator.RunInfo

	// Snapshots is what the repository holds. Populated only when asked for,
	// because listing them runs a Job — see Snapshots.
	Snapshots []backup.Snapshot
}

// runner returns the orchestrator's task runner, if it has one.
func (s *Service) runner() (backup.Runner, bool) {
	r, ok := s.orch.(backup.Runner)
	return r, ok
}

// backupApp assembles what the backup package needs to know about an app.
//
// The Postgres password is opened here, at the moment a task is built, and
// never stored anywhere else. It goes into the task's Secret and nowhere near
// the pod template.
func (s *Service) backupApp(ctx context.Context, a App) (backup.App, error) {
	out := backup.App{Ref: a.Ref(), Port: a.Port}

	vols, err := s.volumesFor(ctx, s.q, a.ID)
	if err != nil {
		return backup.App{}, err
	}
	for _, v := range vols {
		out.Volumes = append(out.Volumes, backup.Volume{
			Name: v.Name, MountPath: v.MountPath,
		})
	}

	if Source(a.Source) != SourcePostgres {
		return out, nil
	}

	plain, secrets, err := s.envFor(ctx, s.q, a.ID)
	if err != nil {
		return backup.App{}, err
	}

	out.Postgres = true
	out.PostgresUser = plain["POSTGRES_USER"]
	out.PostgresDB = plain["POSTGRES_DB"]
	out.PostgresPassword = secrets["POSTGRES_PASSWORD"]

	// A Postgres app whose credentials cannot be read cannot be dumped, and a
	// schedule built without them would fail nightly with an authentication
	// error — which reads as the database being misconfigured rather than as
	// the backup being misconfigured.
	if out.PostgresUser == "" || out.PostgresDB == "" || out.PostgresPassword == "" {
		return backup.App{}, fmt.Errorf(
			"app: %s looks like a Postgres app but its credentials are not readable, "+
				"so it cannot be backed up", a.Name)
	}
	return out, nil
}

// Backups returns an app's backup configuration and recent runs.
func (s *Service) Backups(ctx context.Context, ownerID, name string) (Backups, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return Backups{}, err
	}

	out := Backups{}

	runner, ok := s.runner()
	if !ok {
		out.Because = "this install's orchestrator cannot run scheduled jobs"
		return out, nil
	}
	out.Available = true

	// Loaded to find out whether one exists, not to use: this reports on
	// configuration and runs nothing, so the credentials it would carry are
	// deliberately not kept past this check.
	if _, err := backup.LoadDestination(ctx, s.q, s.keeper, ownerID); err != nil {
		if errors.Is(err, backup.ErrNoDestination) {
			out.Because = "no backup destination is configured for this install"
			return out, nil
		}
		return Backups{}, err
	}
	out.Configured = true

	ba, err := s.backupApp(ctx, a)
	if err != nil {
		return Backups{}, err
	}
	out.Targets = ba.Targets()
	if len(out.Targets) == 0 {
		out.Because = "this app has no database and no volumes, so there is nothing to copy"
		return out, nil
	}

	policy, err := backup.LoadPolicy(ctx, s.q, a.ID)
	switch {
	case errors.Is(err, backup.ErrNoPolicy):
		// Not an error: an app nobody has switched backups on for is the
		// ordinary case. The default is offered so the form has sensible
		// values in it rather than empty inputs.
		out.Policy = backup.DefaultPolicy()
	case err != nil:
		return Backups{}, err
	default:
		out.Policy = policy
		out.HasPolicy = true
	}

	// Runs come from the cluster. Best effort: a schedule that does not exist
	// yet is the normal state before the first save, and reporting that as an
	// error would make the page unreachable for every app that has never been
	// backed up.
	if out.HasPolicy {
		for _, t := range out.Targets {
			ref := s.scheduleRef(a, ba, t)
			runs, err := runner.ScheduleRuns(ctx, ref, 10)
			if err != nil {
				continue
			}
			out.Runs = append(out.Runs, runs...)
		}
	}

	return out, nil
}

// scheduleRef is the schedule name for one of an app's targets, matching what
// backup.Schedules produces.
func (s *Service) scheduleRef(a App, ba backup.App, t backup.Target) orchestrator.Ref {
	name := backup.ScheduleName(a.Name)
	if len(ba.Targets()) > 1 {
		name += "-" + t.Name
	}
	return orchestrator.Ref{
		Owner:     orchestrator.OwnerID(a.OwnerID),
		Namespace: a.Namespace,
		Name:      name,
	}
}

// SetBackupPolicy stores an app's policy and converges its schedules.
func (s *Service) SetBackupPolicy(
	ctx context.Context, ownerID, name string, p backup.Policy,
) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	runner, ok := s.runner()
	if !ok {
		return errors.New("app: this install cannot run scheduled jobs, so backups " +
			"cannot be switched on")
	}

	dest, err := backup.LoadDestination(ctx, s.q, s.keeper, ownerID)
	if err != nil {
		return err
	}

	// Stored before the cluster is touched, and the cluster converged after.
	// The other order would leave a schedule running against a policy nobody
	// can see or change.
	if err := backup.SavePolicy(ctx, s.q, ownerID, a.ID, p); err != nil {
		return err
	}

	ba, err := s.backupApp(ctx, a)
	if err != nil {
		return err
	}

	// Removed first, so an app whose targets changed does not keep a schedule
	// for a volume it no longer has.
	if err := backup.Remove(ctx, runner, ba); err != nil {
		return err
	}
	if err := backup.Apply(ctx, runner, ba, dest, p); err != nil {
		return err
	}

	s.log.Info("backup policy set", slog.String("app", name),
		slog.Bool("enabled", p.Enabled), slog.String("schedule", p.Schedule))
	return nil
}

// DisableBackups removes an app's policy and its schedules.
//
// What has already been written is left alone. Snapshots outlive the policy
// that made them — deleting them here would mean switching backups off also
// destroys the backups, which is the opposite of what anybody pressing it
// expects.
func (s *Service) DisableBackups(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if err := backup.DeletePolicy(ctx, s.q, ownerID, a.ID); err != nil {
		return err
	}

	if runner, ok := s.runner(); ok {
		ba, err := s.backupApp(ctx, a)
		if err != nil {
			return err
		}
		if err := backup.Remove(ctx, runner, ba); err != nil {
			return err
		}
	}

	s.log.Info("backups disabled", slog.String("app", name))
	return nil
}

// BackupNow takes a backup immediately and returns its log.
func (s *Service) BackupNow(
	ctx context.Context, ownerID, name, target string,
) (orchestrator.TaskResult, error) {
	a, ba, dest, runner, err := s.backupContext(ctx, ownerID, name)
	if err != nil {
		return orchestrator.TaskResult{}, err
	}

	policy, err := backup.LoadPolicy(ctx, s.q, a.ID)
	if errors.Is(err, backup.ErrNoPolicy) {
		// A manual backup of an app with no policy still needs retention
		// settings, because the script applies them. The default is the honest
		// choice: it is what the app would get if somebody switched backups on.
		policy = backup.DefaultPolicy()
	} else if err != nil {
		return orchestrator.TaskResult{}, err
	}

	res, err := backup.RunNow(ctx, runner, ba, dest, policy, target)
	s.log.Info("manual backup finished", slog.String("app", name),
		slog.Bool("succeeded", res.Succeeded))
	return res, err
}

// BackupSnapshots lists what the repository holds for an app.
func (s *Service) BackupSnapshots(
	ctx context.Context, ownerID, name string,
) ([]backup.Snapshot, error) {
	_, ba, dest, runner, err := s.backupContext(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	return backup.Snapshots(ctx, runner, ba, dest)
}

// RestoreBackup writes a snapshot back over an app's live data.
//
// Destructive by definition — that is what a restore is — so the confirmation
// belongs to the surface that calls this, not here. What this guarantees is
// that a restore which could not finish reports as failed rather than leaving
// data half replaced and saying nothing.
func (s *Service) RestoreBackup(
	ctx context.Context, ownerID, name, target, snapshot string,
) (orchestrator.TaskResult, error) {
	_, ba, dest, runner, err := s.backupContext(ctx, ownerID, name)
	if err != nil {
		return orchestrator.TaskResult{}, err
	}

	s.log.Warn("restoring from a backup — this overwrites live data",
		slog.String("app", name), slog.String("snapshot", snapshot))

	res, err := backup.Restore(ctx, runner, ba, dest, target, snapshot)
	s.log.Info("restore finished", slog.String("app", name),
		slog.Bool("succeeded", res.Succeeded))
	return res, err
}

// backupContext gathers what every backup operation needs, and fails with one
// message per missing piece rather than a nil dereference three calls later.
func (s *Service) backupContext(
	ctx context.Context, ownerID, name string,
) (App, backup.App, backup.Destination, backup.Runner, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return App{}, backup.App{}, backup.Destination{}, nil, err
	}
	runner, ok := s.runner()
	if !ok {
		return App{}, backup.App{}, backup.Destination{}, nil,
			errors.New("app: this install cannot run jobs, so backups are unavailable")
	}
	dest, err := backup.LoadDestination(ctx, s.q, s.keeper, ownerID)
	if err != nil {
		return App{}, backup.App{}, backup.Destination{}, nil, err
	}
	ba, err := s.backupApp(ctx, a)
	if err != nil {
		return App{}, backup.App{}, backup.Destination{}, nil, err
	}
	return a, ba, dest, runner, nil
}

// BackupDestination returns the install's destination, with credentials left
// sealed.
//
// The secret is never returned. A settings page shows which bucket is
// configured and asks for the credential again to change it, because a form
// that renders a stored secret puts it in the page source, the browser cache,
// and whatever proxies the response.
func (s *Service) BackupDestination(
	ctx context.Context, ownerID string,
) (backup.Destination, bool, error) {
	d, err := backup.LoadDestination(ctx, s.q, s.keeper, ownerID)
	if errors.Is(err, backup.ErrNoDestination) {
		return backup.Destination{}, false, nil
	}
	if err != nil {
		return backup.Destination{}, false, err
	}
	d.SecretAccessKey = ""
	d.RepoPassword = ""
	return d, true, nil
}

// SetBackupDestination stores where backups go, and reapplies every schedule
// that depends on it.
//
// Reapplying matters: the destination is baked into each schedule's environment
// and its Secret, so a bucket changed without this would leave every existing
// schedule writing to the old one.
func (s *Service) SetBackupDestination(
	ctx context.Context, ownerID string, d backup.Destination,
) error {
	if err := backup.SaveDestination(ctx, s.q, s.keeper, ownerID, d); err != nil {
		return err
	}
	s.log.Info("backup destination set",
		slog.String("bucket", d.Bucket), slog.String("endpoint", d.Endpoint))
	return s.reapplyBackupSchedules(ctx, ownerID)
}

// reapplyBackupSchedules converges every schedule an owner has.
func (s *Service) reapplyBackupSchedules(ctx context.Context, ownerID string) error {
	runner, ok := s.runner()
	if !ok {
		return nil
	}
	dest, err := backup.LoadDestination(ctx, s.q, s.keeper, ownerID)
	if err != nil {
		return err
	}

	rows, err := s.q.ListBackupPolicies(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("app: list backup policies: %w", err)
	}

	for _, row := range rows {
		a, err := s.Get(ctx, ownerID, row.AppName)
		if err != nil {
			// An app that has gone is not a reason to abandon the rest.
			s.log.Warn("skipping backup schedule for a missing app",
				slog.String("app", row.AppName), slog.Any("error", err))
			continue
		}
		ba, err := s.backupApp(ctx, a)
		if err != nil {
			s.log.Warn("could not rebuild a backup schedule",
				slog.String("app", row.AppName), slog.Any("error", err))
			continue
		}

		p := backup.Policy{
			Enabled:     row.BackupPolicy.Enabled,
			Schedule:    row.BackupPolicy.Schedule,
			KeepDaily:   int(row.BackupPolicy.KeepDaily),
			KeepWeekly:  int(row.BackupPolicy.KeepWeekly),
			KeepMonthly: int(row.BackupPolicy.KeepMonthly),
		}
		if err := backup.Apply(ctx, runner, ba, dest, p); err != nil {
			s.log.Warn("could not apply a backup schedule",
				slog.String("app", row.AppName), slog.Any("error", err))
		}
	}
	return nil
}

// ClearBackupDestination removes the destination and every schedule that used
// it.
//
// The schedules go because they cannot work without it: left in place they
// would run nightly with a repository they can no longer reach, failing in a
// way that looks like the storage provider being down.
func (s *Service) ClearBackupDestination(ctx context.Context, ownerID string) error {
	runner, hasRunner := s.runner()
	if hasRunner {
		rows, err := s.q.ListBackupPolicies(ctx, ownerID)
		if err != nil {
			return fmt.Errorf("app: list backup policies: %w", err)
		}
		for _, row := range rows {
			a, err := s.Get(ctx, ownerID, row.AppName)
			if err != nil {
				continue
			}
			ba, err := s.backupApp(ctx, a)
			if err != nil {
				continue
			}
			if err := backup.Remove(ctx, runner, ba); err != nil {
				return err
			}
		}
	}

	if err := s.q.DeleteBackupDestination(ctx, ownerID); err != nil {
		return fmt.Errorf("app: delete backup destination: %w", err)
	}
	s.log.Info("backup destination cleared")
	return nil
}
