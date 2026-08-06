package k8s

import (
	"context"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// How much history a schedule keeps in the cluster.
//
// This is the whole record of whether backups have been working, because
// nothing writes rows about it — see orchestrator.RunInfo. Kubernetes' defaults
// are three successes and one failure, and one failure is too few: the useful
// question is whether last night's failure was the first or the fifth, and with
// one kept there is no way to tell.
const (
	keepSuccessful int32 = 5
	keepFailed     int32 = 5
)

// EnsureSchedule converges a recurring task.
func (o *Orchestrator) EnsureSchedule(
	ctx context.Context, spec orchestrator.ScheduleSpec,
) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := o.ensureTaskSecret(ctx, spec.TaskSpec); err != nil {
		return err
	}

	cron := cronJob(spec)
	jobs := o.client.BatchV1().CronJobs(spec.Namespace)

	_, err := jobs.Create(ctx, cron, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Read-modify-write rather than a blind update, because the live object
		// carries status — the last schedule time, the currently active job —
		// and writing a version without it would discard the record of whether
		// this schedule has ever run.
		existing, getErr := jobs.Get(ctx, spec.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("k8s: read schedule %s: %w", spec.Ref, getErr)
		}
		existing.Spec = cron.Spec
		existing.Labels = cron.Labels
		_, err = jobs.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("k8s: apply schedule %s: %w", spec.Ref, err)
	}
	return nil
}

// DeleteSchedule removes one, tolerating its absence.
//
// The task's Secret goes with it. Left behind it would be a credential to the
// backup repository sitting in a namespace with nothing to read it, which is
// the kind of leftover nobody goes looking for.
func (o *Orchestrator) DeleteSchedule(ctx context.Context, ref orchestrator.Ref) error {
	policy := metav1.DeletePropagationForeground
	err := o.client.BatchV1().CronJobs(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete schedule %s: %w", ref, err)
	}

	err = o.client.CoreV1().Secrets(ref.Namespace).
		Delete(ctx, taskSecretName(ref.Name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete schedule secret %s: %w", ref, err)
	}
	return nil
}

// ScheduleRuns reports what a schedule has done lately, most recent first.
func (o *Orchestrator) ScheduleRuns(
	ctx context.Context, ref orchestrator.Ref, limit int,
) ([]orchestrator.RunInfo, error) {
	if _, err := o.client.BatchV1().CronJobs(ref.Namespace).
		Get(ctx, ref.Name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, orchestrator.ErrNotFound
		}
		return nil, fmt.Errorf("k8s: read schedule %s: %w", ref, err)
	}

	// Selected by label rather than by ownerReference. A CronJob's Jobs do
	// carry one, but reading it means fetching the CronJob's uid and comparing
	// — and the label is written by this package, so it survives the CronJob
	// being recreated with the same name.
	jobs, err := o.client.BatchV1().Jobs(ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: taskLabel + "=" + ref.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("k8s: list runs of %s: %w", ref, err)
	}

	out := make([]orchestrator.RunInfo, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		out = append(out, runInfo(j))
	}

	// Most recent first, which is the order the question is asked in: did last
	// night's backup work.
	slices.SortFunc(out, func(a, b orchestrator.RunInfo) int {
		return b.StartedAt.Compare(a.StartedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func runInfo(j batchv1.Job) orchestrator.RunInfo {
	info := orchestrator.RunInfo{Name: j.Name}

	// StartTime is set by the Job controller once a pod is created. Falling
	// back to creation time keeps a run that has not started yet in the list,
	// rather than sorting it to 1970 and burying it.
	if j.Status.StartTime != nil {
		info.StartedAt = j.Status.StartTime.Time
	} else {
		info.StartedAt = j.CreationTimestamp.Time
	}

	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			ok := true
			info.Succeeded = &ok
		case batchv1.JobFailed:
			no := false
			info.Succeeded = &no
		default:
			continue
		}
		if !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime.Time
			info.FinishedAt = &t
		}
	}

	// CompletionTime is more accurate than the condition's transition time, so
	// prefer it where the Job has one.
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.Time
		info.FinishedAt = &t
	}
	return info
}

// cronJob wraps a task's pod in a schedule.
func cronJob(spec orchestrator.ScheduleSpec) *batchv1.CronJob {
	labels := orchestrator.ObjectLabels(spec.Ref)
	labels[taskLabel] = spec.Name

	var backoff int32
	deadline := int64(spec.EffectiveTimeout().Seconds())

	// Forbid, not Replace. Two backups writing one restic repository at once is
	// the case its locking exists for, and the second would spend the window
	// waiting on a lock rather than backing anything up. Replace is worse
	// still: it would kill a backup that is merely slow, every time, and the
	// larger the data the more certain that becomes.
	concurrency := batchv1.ForbidConcurrent

	// How late a missed run may still fire. Without it Kubernetes counts every
	// missed schedule since the last success, and a CronJob that was suspended
	// or whose controller was down for a day refuses to run again at all,
	// reporting "too many missed start times". Backups stopping permanently
	// after an outage is exactly the wrong response to an outage.
	startDeadline := int64(3600)

	successful := keepSuccessful
	failed := keepFailed

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.Name, Namespace: spec.Namespace, Labels: labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   spec.Cron,
			Suspend:                    boolPtr(spec.Suspended),
			ConcurrencyPolicy:          concurrency,
			StartingDeadlineSeconds:    &startDeadline,
			SuccessfulJobsHistoryLimit: &successful,
			FailedJobsHistoryLimit:     &failed,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:          &backoff,
					ActiveDeadlineSeconds: &deadline,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       taskPodSpec(spec.TaskSpec),
					},
				},
			},
		},
	}
}

// Interface assertions, so a change to the seam fails here rather than at the
// call site that asserts for it and quietly finds nothing.
var _ orchestrator.Runner = (*Orchestrator)(nil)
