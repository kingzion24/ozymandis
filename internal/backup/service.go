package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// scheduleName is the CronJob an app's backups run under.
//
// Derived from the app name rather than stored, so the schedule for an app is
// findable from the app alone — which is what lets a reconcile at startup
// delete the schedules of apps that no longer exist.
func scheduleName(app string) string { return app + "-backup" }

// ScheduleName is the exported form, for callers reconciling schedules.
func ScheduleName(app string) string { return scheduleName(app) }

// restoreName and snapshotsName are one-off tasks. Named per app and reused
// between runs: RunTask deletes any previous Job of the same name first, so a
// stable name means at most one restore of an app can be in flight, which is
// the correct number.
func restoreName(app string) string   { return app + "-restore" }
func snapshotsName(app string) string { return app + "-snapshots" }

// App is what this package needs to know about a workload to back it up.
//
// A struct of the few relevant fields rather than importing the app package,
// because app already depends on this one — and a backup that needed the whole
// app record would be a backup coupled to every future change to it.
type App struct {
	Ref orchestrator.Ref

	// Postgres reports that this app is a database this engine provisioned, so
	// it can be dumped rather than copied file by file.
	Postgres bool

	// PostgresUser, PostgresDB and PostgresPassword are how to reach it. The
	// password is the app's own generated secret, unsealed by the caller.
	PostgresUser     string
	PostgresDB       string
	PostgresPassword string

	// Port the database listens on.
	Port int32

	// Volumes are the app's storage, by name.
	Volumes []Volume
}

// Volume is one piece of an app's storage.
type Volume struct {
	Name      string
	MountPath string
}

// Targets returns what would be copied for this app.
//
// A Postgres app yields its dump and *not* its volume, which looks like an
// omission and is the most considered decision in this file. The volume holds
// that same database's files, and copying them from a running server produces a
// backup that is consistent only if the copy happened to fall between
// checkpoints. Keeping both would mean holding two backups of one database
// where one of them is unreliable and neither is labelled — and at restore
// time, no way to tell which is which.
func (a App) Targets() []Target {
	if a.Postgres {
		return []Target{{
			Kind:    KindPostgres,
			Name:    a.PostgresDB,
			Service: a.Ref.Name,
			Port:    a.Port,
		}}
	}

	out := make([]Target, 0, len(a.Volumes))
	for _, v := range a.Volumes {
		out = append(out, Target{
			Kind:      KindVolume,
			Name:      v.Name,
			MountPath: v.MountPath,
		})
	}
	return out
}

// task builds the spec shared by scheduled and manual runs.
func (a App) task(d Destination, name, script string, t Target, readOnly bool) orchestrator.TaskSpec {
	spec := orchestrator.TaskSpec{
		Ref: orchestrator.Ref{
			Owner:     a.Ref.Owner,
			Namespace: a.Ref.Namespace,
			Name:      name,
		},
		Image: Image,

		// sh -c because these scripts are shell: pipelines, a conditional
		// init, and pipefail. The script is generated here rather than
		// assembled from anything a person typed.
		Command:   []string{"/bin/sh", "-c", script},
		Env:       Env(d, a.Ref.Name),
		Secrets:   Secrets(d),
		App:       a.Ref.Name,
		RunAsUser: RunAsUser,
		FSGroup:   FSGroup,
	}

	switch t.Kind {
	case KindPostgres:
		spec.Env["PGUSER"] = a.PostgresUser
		spec.Env["PGDATABASE"] = a.PostgresDB
		spec.Secrets = PostgresSecrets(d, a.PostgresPassword)

	case KindVolume:
		// fsGroup comes from the app rather than the backup image: the files
		// on the claim are owned by whatever uid the app runs as, and a task
		// in a different group cannot read them.
		spec.Mounts = []orchestrator.TaskMount{{
			Volume:    t.Name,
			MountPath: dataMount,
			ReadOnly:  readOnly,
		}}
	}
	return spec
}

// Schedules returns the recurring tasks that carry out a policy.
//
// One per target. A single job doing every target would report one exit status
// for several independent questions, so a failed volume backup and a failed
// database dump would be indistinguishable — and the retention pass for the
// one that worked would not run.
func (a App) Schedules(d Destination, p Policy) ([]orchestrator.ScheduleSpec, error) {
	targets := a.Targets()
	if len(targets) == 0 {
		return nil, ErrNothingToBackUp
	}

	out := make([]orchestrator.ScheduleSpec, 0, len(targets))
	for _, t := range targets {
		name := scheduleName(a.Ref.Name)
		if len(targets) > 1 {
			name = scheduleName(a.Ref.Name) + "-" + t.Name
		}

		// Read-only: a backup that can write to the volume it is copying is a
		// backup that can corrupt the thing it exists to protect.
		spec := a.task(d, name, BackupScript(p, t), t, true)
		out = append(out, orchestrator.ScheduleSpec{
			TaskSpec:  spec,
			Cron:      p.Schedule,
			Suspended: !p.Enabled,
		})
	}
	return out, nil
}

// Runner is the part of the orchestrator this package needs.
//
// Declared here rather than taking the whole Orchestrator so that a caller
// holding an orchestrator that cannot run tasks finds out by a failed type
// assertion at wiring time, not by a backup that silently never happens.
type Runner interface {
	RunTask(ctx context.Context, spec orchestrator.TaskSpec) (orchestrator.TaskResult, error)
	EnsureSchedule(ctx context.Context, spec orchestrator.ScheduleSpec) error
	DeleteSchedule(ctx context.Context, ref orchestrator.Ref) error
	ScheduleRuns(ctx context.Context, ref orchestrator.Ref, limit int) ([]orchestrator.RunInfo, error)
}

// Apply converges an app's schedules to its policy.
func Apply(ctx context.Context, r Runner, a App, d Destination, p Policy) error {
	specs, err := a.Schedules(d, p)
	if err != nil {
		return err
	}
	for _, s := range specs {
		if err := r.EnsureSchedule(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes an app's schedules.
//
// Every name a policy could have produced is deleted, not just the ones the
// current policy would produce. An app that had two volumes and now has one
// would otherwise keep a schedule for the volume it no longer has — a job
// failing nightly against a claim that does not exist.
func Remove(ctx context.Context, r Runner, a App) error {
	names := map[string]bool{scheduleName(a.Ref.Name): true}
	for _, t := range a.Targets() {
		names[scheduleName(a.Ref.Name)+"-"+t.Name] = true
	}
	for _, v := range a.Volumes {
		names[scheduleName(a.Ref.Name)+"-"+v.Name] = true
	}

	for name := range names {
		ref := orchestrator.Ref{
			Owner: a.Ref.Owner, Namespace: a.Ref.Namespace, Name: name,
		}
		if err := r.DeleteSchedule(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// RunNow takes a backup immediately, and returns its log.
//
// The manual path matters more than it looks: it is how somebody finds out
// their destination is misconfigured at the moment they configure it, rather
// than at 3am from a Job condition nobody reads.
func RunNow(
	ctx context.Context, r Runner, a App, d Destination, p Policy, target string,
) (orchestrator.TaskResult, error) {
	t, ok := a.target(target)
	if !ok {
		return orchestrator.TaskResult{}, fmt.Errorf(
			"backup: %s has nothing called %q to back up", a.Ref.Name, target)
	}
	spec := a.task(d, scheduleName(a.Ref.Name)+"-now", BackupScript(p, t), t, true)
	return r.RunTask(ctx, spec)
}

// Snapshots lists what the repository holds for this app.
func Snapshots(
	ctx context.Context, r Runner, a App, d Destination,
) ([]Snapshot, error) {
	spec := a.task(d, snapshotsName(a.Ref.Name), SnapshotsScript(), Target{}, true)

	// Short: this reads a small index and returns. A listing that takes minutes
	// is a broken destination, and waiting an hour to be told so is not better
	// than being told in two.
	spec.Timeout = 5 * time.Minute

	res, err := r.RunTask(ctx, spec)
	if err != nil {
		return nil, err
	}
	if !res.Succeeded {
		return nil, fmt.Errorf("backup: could not list snapshots:\n%s", res.Output)
	}
	return parseSnapshots(res.Output)
}

// Restore writes a snapshot back over the app's live data.
//
// Deliberately not wrapped in any "are you sure" of its own — that belongs to
// the surface a person clicks. What this does guarantee is that it fails loudly
// rather than partially: the scripts stop at the first error, so a restore that
// could not complete leaves an error rather than a database half replaced and
// reported as restored.
func Restore(
	ctx context.Context, r Runner, a App, d Destination, target, snapshot string,
) (orchestrator.TaskResult, error) {
	t, ok := a.target(target)
	if !ok {
		return orchestrator.TaskResult{}, fmt.Errorf(
			"backup: %s has nothing called %q to restore", a.Ref.Name, target)
	}
	if snapshot == "" {
		snapshot = "latest"
	}

	// Writable, unlike a backup: this is the one task that must be able to
	// change the volume.
	spec := a.task(d, restoreName(a.Ref.Name), RestoreScript(t, snapshot), t, false)
	return r.RunTask(ctx, spec)
}

func (a App) target(name string) (Target, bool) {
	targets := a.Targets()
	// An app with exactly one target does not make somebody name it. Restoring
	// "the database" of an app that has one database should not require
	// knowing what this engine happened to call it.
	if name == "" && len(targets) == 1 {
		return targets[0], true
	}
	for _, t := range targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// resticSnapshot is the subset of restic's JSON this reads.
//
// Named fields rather than a map, so a change to restic's output that drops one
// of these fails to unmarshal into something obviously empty rather than
// silently producing snapshots with no id.
type resticSnapshot struct {
	ID    string    `json:"short_id"`
	Time  time.Time `json:"time"`
	Tags  []string  `json:"tags"`
	Stats struct {
		TotalBytesProcessed int64 `json:"total_bytes_processed"`
	} `json:"summary"`
}

// parseSnapshots reads restic's JSON listing out of a task's log.
//
// The log carries the preamble's output too, so the JSON is found rather than
// assumed to start at byte zero — an assumption that would break the first time
// the repository needed initialising and the script said so.
func parseSnapshots(output string) ([]Snapshot, error) {
	start := strings.Index(output, "[")
	if start < 0 {
		// An empty repository prints an empty array, so no array at all means
		// the command did not get that far.
		return nil, fmt.Errorf("backup: no snapshot listing in the output:\n%s", output)
	}

	var raw []resticSnapshot
	if err := json.Unmarshal([]byte(output[start:]), &raw); err != nil {
		return nil, fmt.Errorf("backup: could not read the snapshot listing: %w", err)
	}

	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		out = append(out, Snapshot{
			ID:        s.ID,
			Time:      s.Time,
			Tags:      s.Tags,
			SizeBytes: s.Stats.TotalBytesProcessed,
		})
	}
	return out, nil
}
