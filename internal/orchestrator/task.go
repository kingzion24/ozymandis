package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskSpec is work that runs to completion, rather than a workload that stays
// up.
//
// Separate from AppSpec because almost nothing they carry means the same
// thing. A task has no replicas, no hostname, no readiness — asking whether a
// backup is "ready to serve traffic" is not a question — and it has one
// property an app must never have: it ends, and whether it ended well is the
// only thing anybody wants to know about it.
//
// The security posture is not among its fields, for the same reason AppSpec
// does not carry it. A task runs under the same restricted context every
// workload does, and there is no field here that could ask for otherwise.
type TaskSpec struct {
	Ref

	Image string

	// Command is the whole command line, already argv. No shell runs between
	// this and the process unless the task names one itself.
	Command []string

	Env map[string]string

	// Secrets become a Kubernetes Secret read with envFrom, so credentials do
	// not appear in the pod template — which for a backup task is the
	// credential to the place every backup is kept.
	Secrets map[string]string

	// App is the workload whose storage Mounts refer to.
	//
	// Not the task's own name: a backup of "db" is a task called something
	// like "db-backup", and its volumes are the ones "db" owns. Naming the app
	// explicitly is what lets those two differ, and what stops a task from
	// being able to reach a volume by guessing at a name.
	App string

	// Mounts are existing volumes to attach, by the name they were created
	// with. A task does not create storage: it reads or writes storage some
	// app already owns, and a task that could provision would be a second
	// place volumes come from.
	Mounts []TaskMount

	// RegistryAuth is a Docker config.json for pulling Image, when it comes
	// from a registry that needs one.
	//
	// Carried on the task rather than assumed to exist in the namespace,
	// because a release task runs BEFORE the deploy that would have put the
	// pull credential there. On an app's first deploy the namespace has no
	// pull secret yet, so a task relying on one would fail with
	// ImagePullBackOff on exactly the deploy a release command matters most —
	// the one that creates the database it is about to migrate.
	RegistryAuth []byte

	RunAsUser int64
	FSGroup   int64

	// Timeout caps one run. Zero means DefaultTaskTimeout.
	//
	// A cap rather than none, because the failure this prevents is silent: a
	// backup that hangs holds its schedule's concurrency slot, and every run
	// after it is skipped rather than queued. The symptom is backups quietly
	// stopping, which is the symptom of no backups at all.
	Timeout time.Duration
}

// TaskMount attaches storage an app already owns.
type TaskMount struct {
	// Volume is the app's name for it, as given in VolumeSpec.
	Volume string

	MountPath string

	// ReadOnly is what a backup wants and a restore cannot have. Named rather
	// than inferred from the task, because the implementation cannot tell them
	// apart and guessing wrong in the permissive direction means a backup that
	// can corrupt what it is backing up.
	ReadOnly bool
}

// DefaultTaskTimeout caps a task that names no timeout of its own.
const DefaultTaskTimeout = time.Hour

// ScheduleSpec is a TaskSpec that runs on a cron schedule.
type ScheduleSpec struct {
	TaskSpec

	// Cron is a five-field expression in the cluster's timezone.
	Cron string

	// Suspended keeps the schedule in place and stops it firing. Distinct from
	// deleting it: an owner switching backups off for an afternoon should get
	// their schedule back, not have to retype it.
	Suspended bool
}

// TaskResult is what a finished task produced.
type TaskResult struct {
	// Output is the task's combined log, which for the tasks this engine runs
	// is also the answer — a restic snapshot listing is stdout, not a status
	// field.
	Output string

	// Succeeded reports whether it exited zero. The log is returned either way:
	// a failed backup's reason is in it, and dropping the log on failure is how
	// a job becomes impossible to debug from the dashboard.
	Succeeded bool
}

// Runner runs work that finishes.
//
// Optional, and asserted for rather than required, in the same way NodeManager
// is. An orchestrator that cannot run a Job is still a working orchestrator;
// the surfaces that need one report themselves unavailable with a reason
// instead of failing when somebody presses them.
type Runner interface {
	// RunTask runs a task to completion and returns what it produced.
	//
	// Synchronous, like Build, and for the same reason: whether to wait is the
	// caller's decision, and an interface that decided would force one caller
	// to work around it.
	RunTask(ctx context.Context, spec TaskSpec) (TaskResult, error)

	// EnsureSchedule converges a recurring task. Safe to call repeatedly with
	// the same spec.
	EnsureSchedule(ctx context.Context, spec ScheduleSpec) error

	// DeleteSchedule removes one. It returns nil if already gone.
	DeleteSchedule(ctx context.Context, ref Ref) error

	// ScheduleRuns reports what a schedule has done lately, most recent first.
	// It returns ErrNotFound if the schedule does not exist.
	ScheduleRuns(ctx context.Context, ref Ref, limit int) ([]RunInfo, error)
}

// RunInfo is one execution of a schedule.
//
// Read from the runtime rather than recorded as rows when a job reports in.
// A backup job runs in the cluster with no route back to this process, so
// "recorded" would mean a callback it could fail to make — and a run that
// finished without saying so is indistinguishable from one that never ran. The
// cluster already knows; asking it cannot disagree with what happened.
type RunInfo struct {
	Name       string
	StartedAt  time.Time
	FinishedAt *time.Time

	// Succeeded is nil while the run is still going.
	Succeeded *bool
}

// Finished reports whether the run has ended, either way.
func (r RunInfo) Finished() bool { return r.Succeeded != nil }

// Validate checks a task well enough to avoid sending nonsense to a runtime.
func (s TaskSpec) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return err
	}
	if s.Image == "" {
		return errors.New("task spec: image is required")
	}
	if len(s.Command) == 0 {
		return errors.New("task spec: command is required")
	}
	if s.Command[0] == "" {
		return errors.New("task spec: command must not start with an empty argument")
	}
	if s.Timeout < 0 {
		return fmt.Errorf("task spec: timeout must not be negative, got %s", s.Timeout)
	}

	if len(s.Mounts) > 0 {
		if err := ValidateDNSLabel("mount app", s.App); err != nil {
			return err
		}
	}

	seen := make(map[string]bool, len(s.Mounts))
	for _, m := range s.Mounts {
		if err := ValidateDNSLabel("mount volume", m.Volume); err != nil {
			return err
		}
		if !strings.HasPrefix(m.MountPath, "/") || m.MountPath == "/" {
			return fmt.Errorf("task spec: mount path %q must be absolute and not /", m.MountPath)
		}
		if seen[m.MountPath] {
			return fmt.Errorf("task spec: two volumes mounted at %q", m.MountPath)
		}
		seen[m.MountPath] = true
	}
	return nil
}

// EffectiveTimeout is the cap that applies.
func (s TaskSpec) EffectiveTimeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTaskTimeout
	}
	return s.Timeout
}

// cronFields is the number of fields in the expression Kubernetes accepts.
const cronFields = 5

// Validate checks a schedule.
//
// The cron expression is checked for shape only. Kubernetes parses it for
// real, and a second parser here would be a second opinion that has to agree
// with the first — the failure mode being a schedule this accepts and the
// cluster rejects, reported as the CronJob failing to create rather than as
// the expression being wrong.
func (s ScheduleSpec) Validate() error {
	if err := s.TaskSpec.Validate(); err != nil {
		return err
	}
	if len(strings.Fields(s.Cron)) != cronFields {
		return fmt.Errorf("schedule spec: %q is not a %d-field cron expression",
			s.Cron, cronFields)
	}
	return nil
}
