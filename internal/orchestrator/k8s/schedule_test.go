package k8s

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

func testSchedule() orchestrator.ScheduleSpec {
	return orchestrator.ScheduleSpec{
		TaskSpec: orchestrator.TaskSpec{
			Ref: orchestrator.Ref{
				Owner: "owner-1", Namespace: "ozymandis-demo", Name: "db-backup",
			},
			Image:   "ghcr.io/example/backup:1",
			Command: []string{"/bin/sh", "-c", "restic backup /data"},
			Env:     map[string]string{"RESTIC_REPOSITORY": "s3:https://x/y"},
			Secrets: map[string]string{"RESTIC_PASSWORD": "hunter2hunter2"},
		},
		Cron: "17 3 * * *",
	}
}

func TestEnsureScheduleCreatesACronJob(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}

	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Schedule != "17 3 * * *" {
		t.Errorf("schedule = %q, want the one asked for", cj.Spec.Schedule)
	}

	// Forbid, so two backups never write one repository at once. Replace would
	// kill a backup that is merely slow, every single time.
	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("concurrency = %q, want Forbid", cj.Spec.ConcurrencyPolicy)
	}

	// Without a starting deadline, a controller that was down for a day
	// refuses to run the job ever again — backups stopping permanently as the
	// response to an outage.
	if cj.Spec.StartingDeadlineSeconds == nil {
		t.Error("no starting deadline: a missed window would stop this schedule for good")
	}
}

// The credential must be in a Secret, never in the pod template. A backup
// task's credential opens every backup ever taken of the app, and a pod
// template is what people paste into issues.
func TestScheduleKeepsCredentialsOutOfThePodTemplate(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}

	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	container := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	for _, e := range container.Env {
		if e.Value == "hunter2hunter2" {
			t.Fatalf("the repository password is a literal in the pod template (%s)", e.Name)
		}
	}
	if len(container.EnvFrom) != 1 ||
		container.EnvFrom[0].SecretRef.Name != taskSecretName(spec.Name) {
		t.Fatalf("envFrom = %+v, want the task's own Secret", container.EnvFrom)
	}

	sec, err := client.CoreV1().Secrets(spec.Namespace).
		Get(ctx, taskSecretName(spec.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get task secret: %v", err)
	}
	if sec.StringData["RESTIC_PASSWORD"] != "hunter2hunter2" {
		t.Error("the password did not reach the Secret")
	}
}

// A backup task runs with the credential to every backup it has ever made. The
// service account token is the one thing worth being certain is not mounted
// beside it.
func TestScheduleMountsNoServiceAccountToken(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}
	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("a backup pod would carry a Kubernetes API token")
	}

	c := pod.Containers[0]
	if c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Error("the task container is not pinned to a non-root user")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the task container has a writable root filesystem")
	}
}

// The mount a backup reads must be read-only. A backup that can write to the
// volume it is copying can corrupt the thing it exists to protect.
func TestScheduleMountsAVolumeReadOnly(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSchedule()
	spec.App = "db"
	spec.Mounts = []orchestrator.TaskMount{
		{Volume: "data", MountPath: "/data", ReadOnly: true},
	}

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}
	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	var found bool
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		found = true
		// The claim belongs to the app, not to the task — the two have
		// different names and a task must not be able to reach a volume by
		// guessing.
		if got := v.PersistentVolumeClaim.ClaimName; got != claimName("db", "data") {
			t.Errorf("claim = %q, want the app's own", got)
		}
		if !v.PersistentVolumeClaim.ReadOnly {
			t.Error("the claim is mounted writable by a backup")
		}
	}
	if !found {
		t.Fatal("no claim was mounted")
	}

	for _, m := range pod.Containers[0].VolumeMounts {
		if m.MountPath == "/data" && !m.ReadOnly {
			t.Error("the volume mount is writable by a backup")
		}
	}
}

// Reapplying must not discard status. The CronJob's status is where the record
// of whether backups have been running actually lives.
func TestEnsureScheduleIsIdempotentAndKeepsStatus(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("first EnsureSchedule: %v", err)
	}

	// Stand in for the controller having run it.
	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	now := metav1.Now()
	cj.Status.LastScheduleTime = &now
	if _, err := client.BatchV1().CronJobs(spec.Namespace).
		Update(ctx, cj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	spec.Cron = "0 4 * * *"
	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("second EnsureSchedule: %v", err)
	}

	got, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if got.Spec.Schedule != "0 4 * * *" {
		t.Errorf("schedule = %q, want the updated one", got.Spec.Schedule)
	}
	if got.Status.LastScheduleTime == nil {
		t.Error("reapplying discarded the record of when this last ran")
	}
}

// Suspending keeps the schedule and stops it firing. Deleting it instead would
// mean an owner pausing backups for an afternoon has to retype the policy.
func TestASuspendedScheduleIsKeptAndNotFired(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSchedule()
	spec.Suspended = true
	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}

	cj, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Fatal("a disabled policy produced a schedule that still fires")
	}
}

// Deleting a schedule takes its Secret with it. Left behind it is a credential
// to the backup repository sitting in a namespace with nothing to read it.
func TestDeleteScheduleRemovesTheCredential(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}
	if err := o.DeleteSchedule(ctx, spec.Ref); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}

	if _, err := client.BatchV1().CronJobs(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the schedule gone, got %v", err)
	}
	if _, err := client.CoreV1().Secrets(spec.Namespace).
		Get(ctx, taskSecretName(spec.Name), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the repository credential outlived its schedule: %v", err)
	}
}

func TestDeleteScheduleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	if err := o.DeleteSchedule(ctx, testSchedule().Ref); err != nil {
		t.Fatalf("DeleteSchedule on a schedule that never existed: %v", err)
	}
}

func TestScheduleRunsIsNotFoundForAnUnknownSchedule(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	_, err := o.ScheduleRuns(ctx, testSchedule().Ref, 10)
	if err == nil {
		t.Fatal("ScheduleRuns invented a schedule that does not exist")
	}
}

// A run that has not finished must not report as succeeded. Reporting an
// in-flight backup as done is how a backup that later failed gets remembered as
// one that worked.
func TestScheduleRunsReportsUnfinishedRunsAsUnfinished(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}

	started := metav1.Now()
	if _, err := client.BatchV1().Jobs(spec.Namespace).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-backup-1",
			Namespace: spec.Namespace,
			Labels:    map[string]string{taskLabel: spec.Name},
		},
		Status: batchv1.JobStatus{StartTime: &started},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	runs, err := o.ScheduleRuns(ctx, spec.Ref, 10)
	if err != nil {
		t.Fatalf("ScheduleRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	if runs[0].Finished() {
		t.Error("a running backup was reported as finished")
	}
	if runs[0].Succeeded != nil {
		t.Error("a running backup was given an outcome")
	}
}

func TestScheduleRunsReportsOutcomesMostRecentFirst(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSchedule()

	if err := o.EnsureSchedule(ctx, spec); err != nil {
		t.Fatalf("EnsureSchedule: %v", err)
	}

	older := metav1.NewTime(metav1.Now().Add(-2 * 60 * 60 * 1e9))
	newer := metav1.Now()

	for _, tc := range []struct {
		name    string
		started metav1.Time
		cond    batchv1.JobConditionType
	}{
		{"db-backup-old", older, batchv1.JobComplete},
		{"db-backup-new", newer, batchv1.JobFailed},
	} {
		if _, err := client.BatchV1().Jobs(spec.Namespace).Create(ctx, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tc.name,
				Namespace: spec.Namespace,
				Labels:    map[string]string{taskLabel: spec.Name},
			},
			Status: batchv1.JobStatus{
				StartTime: &tc.started,
				Conditions: []batchv1.JobCondition{
					{Type: tc.cond, Status: "True", LastTransitionTime: tc.started},
				},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create run %s: %v", tc.name, err)
		}
	}

	runs, err := o.ScheduleRuns(ctx, spec.Ref, 10)
	if err != nil {
		t.Fatalf("ScheduleRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}

	// Most recent first: "did last night's backup work" is the question this
	// list is read to answer.
	if runs[0].Name != "db-backup-new" {
		t.Fatalf("first run = %q, want the most recent", runs[0].Name)
	}
	if runs[0].Succeeded == nil || *runs[0].Succeeded {
		t.Error("the failed run was not reported as failed")
	}
	if runs[1].Succeeded == nil || !*runs[1].Succeeded {
		t.Error("the successful run was not reported as successful")
	}
}
