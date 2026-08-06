package k8s

import (
	"context"
	"fmt"
	"io"
	stdmaps "maps"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// taskSecretName is where a task's credentials live, per task rather than per
// app.
//
// Suffixed with the task's own name so a backup's storage credential and the
// app's environment do not land in the same object. They have different
// lifetimes and very different blast radii: an app's environment opens that
// app, and a backup credential opens every backup ever taken of it.
func taskSecretName(task string) string { return task + "-task" }

// taskLabel marks the objects this file creates, so a run can be found again
// without knowing the generated name of its pod.
const taskLabel = "ozymandis/task"

// RunTask runs a task to completion and returns its output.
//
// Implemented as a Job rather than a bare Pod. A Pod that fails to schedule
// stays Pending with no controller to explain it or give up; a Job carries the
// deadline and the backoff, so a task that cannot run ends rather than hangs.
func (o *Orchestrator) RunTask(
	ctx context.Context, spec orchestrator.TaskSpec,
) (orchestrator.TaskResult, error) {
	if err := spec.Validate(); err != nil {
		return orchestrator.TaskResult{}, err
	}

	if err := o.ensureTaskSecret(ctx, spec); err != nil {
		return orchestrator.TaskResult{}, err
	}

	// The credential to pull spec.Image, when one is needed. Written here
	// rather than relied on from the namespace: a release task runs before the
	// deploy that would have put it there, so on a first deploy there is none.
	if err := o.ensureTaskPullSecret(ctx, spec); err != nil {
		return orchestrator.TaskResult{}, err
	}

	job := taskJob(spec)

	// Deleted first so a re-run is not refused by the leftover of the previous
	// one. Propagation is Foreground: without it the old Job's pods outlive it
	// briefly and the new Job adopts nothing while its name is still taken.
	if err := o.deleteTaskJob(ctx, spec.Namespace, spec.Name); err != nil {
		return orchestrator.TaskResult{}, err
	}

	if _, err := o.client.BatchV1().Jobs(spec.Namespace).
		Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return orchestrator.TaskResult{}, fmt.Errorf("k8s: create task %s: %w", spec.Ref, err)
	}

	// The Job outlives this call only if we are interrupted. TTL on the Job
	// itself is the backstop for that; this is the ordinary path.
	defer func() {
		// A fresh context: the caller's may already be cancelled, and that is
		// exactly the case where the cleanup matters most.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := o.deleteTaskJob(cleanup, spec.Namespace, spec.Name); err != nil {
			o.log.Warn("could not clean up task job",
				"task", spec.Ref.String(), "error", err)
		}
	}()

	done, waitErr := o.awaitJob(ctx, spec.Namespace, spec.Name, spec.EffectiveTimeout())

	// Read the log before deciding what to return. A failed task's reason is in
	// its output, and returning the error without it leaves the dashboard
	// saying "backup failed" and nothing else.
	out := o.taskLog(ctx, spec.Namespace, spec.Name)

	if waitErr != nil {
		return orchestrator.TaskResult{Output: out}, waitErr
	}
	return orchestrator.TaskResult{Output: out, Succeeded: done}, nil
}

// awaitJob blocks until the Job finishes, and reports whether it succeeded.
func (o *Orchestrator) awaitJob(
	ctx context.Context, namespace, name string, timeout time.Duration,
) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var succeeded bool
	err := wait.PollUntilContextCancel(ctx, time.Second, true,
		func(ctx context.Context) (bool, error) {
			job, err := o.client.BatchV1().Jobs(namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				// Not found mid-poll means something else deleted it. Treated
				// as an error rather than as completion: reporting a task that
				// vanished as successful is how a backup that never ran gets
				// recorded as one that did.
				return false, fmt.Errorf("k8s: poll task %s/%s: %w", namespace, name, err)
			}
			for _, c := range job.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					continue
				}
				switch c.Type {
				case batchv1.JobComplete:
					succeeded = true
					return true, nil
				case batchv1.JobFailed:
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return false, fmt.Errorf("k8s: task %s/%s did not finish: %w", namespace, name, err)
	}
	return succeeded, nil
}

// taskLog returns the output of a task's pod.
//
// Best effort. A pod evicted before it wrote anything has no log, and that is
// not a second failure to report on top of the one the caller already has.
func (o *Orchestrator) taskLog(ctx context.Context, namespace, name string) string {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: taskLabel + "=" + name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}

	// The most recent, because a retried task has more than one and the last
	// attempt is the one whose outcome was reported.
	pod := slices.MaxFunc(pods.Items, func(a, b corev1.Pod) int {
		return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
	})

	stream, err := o.client.CoreV1().Pods(namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()

	// Whatever arrived before a read error is still worth having: a task
	// killed mid-write has its reason in the part that made it out.
	out, _ := io.ReadAll(stream)
	return string(out)
}

func (o *Orchestrator) deleteTaskJob(ctx context.Context, namespace, name string) error {
	policy := metav1.DeletePropagationForeground
	err := o.client.BatchV1().Jobs(namespace).
		Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete task %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureTaskSecret writes the task's credentials, or removes the object when it
// has none.
//
// Removing matters: a task that used to carry a credential and no longer does
// would otherwise keep reading the old one from a Secret nobody updated.
func (o *Orchestrator) ensureTaskSecret(
	ctx context.Context, spec orchestrator.TaskSpec,
) error {
	name := taskSecretName(spec.Name)
	secrets := o.client.CoreV1().Secrets(spec.Namespace)

	if len(spec.Secrets) == 0 {
		err := secrets.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: delete task secret %s: %w", spec.Ref, err)
		}
		return nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: spec.Namespace,
			Labels:    orchestrator.ObjectLabels(spec.Ref),
		},
		StringData: spec.Secrets,
	}

	_, err := secrets.Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("k8s: write task secret %s: %w", spec.Ref, err)
	}
	return nil
}

// taskPodSpec is the pod a task runs in, shared by the one-off Job and the
// CronJob so a scheduled backup and a manual one are the same thing running.
func taskPodSpec(spec orchestrator.TaskSpec) corev1.PodSpec {
	pod := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,

		// A task has no reason to talk to the Kubernetes API, and a backup task
		// runs with the credential to every backup ever taken — so the token
		// that would let it reach the API is the one thing worth being sure is
		// not there.
		AutomountServiceAccountToken: boolPtr(false),

		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   boolPtr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Volumes: []corev1.Volume{{
			Name:         tmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}

	if len(spec.RegistryAuth) > 0 {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: orchestrator.PullSecretName},
		}
	}

	if spec.RunAsUser > 0 {
		pod.SecurityContext.RunAsUser = int64Ptr(spec.RunAsUser)
		pod.SecurityContext.RunAsGroup = int64Ptr(spec.RunAsUser)
	}
	// Same rule as a workload's: a claim is provisioned owned by root, and
	// without this a non-root task cannot read the volume it was pointed at.
	if spec.FSGroup > 0 && len(spec.Mounts) > 0 {
		pod.SecurityContext.FSGroup = int64Ptr(spec.FSGroup)
	}

	container := corev1.Container{
		Name:            "task",
		Image:           spec.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         spec.Command,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Privileged:               boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: tmpVolumeName, MountPath: "/tmp"}},
	}

	// Sorted, so the same spec produces the same object every time. An unsorted
	// map here would rewrite the CronJob on every reconcile, which reads in the
	// event log as the schedule changing constantly.
	for _, k := range slices.Sorted(stdmaps.Keys(spec.Env)) {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	if len(spec.Secrets) > 0 {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: taskSecretName(spec.Name),
				},
			},
		}}
	}

	for _, m := range spec.Mounts {
		name := "vol-" + m.Volume
		pod.Volumes = append(pod.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName(spec.App, m.Volume),
					ReadOnly:  m.ReadOnly,
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: name, MountPath: m.MountPath, ReadOnly: m.ReadOnly,
		})
	}

	pod.Containers = []corev1.Container{container}
	return pod
}

// taskJob wraps the pod as a one-off Job.
func taskJob(spec orchestrator.TaskSpec) *batchv1.Job {
	labels := orchestrator.ObjectLabels(spec.Ref)
	labels[taskLabel] = spec.Name

	// Never retried. These tasks are backups and restores: a failed one should
	// be looked at, not attempted again automatically against the same
	// repository. Kubernetes' own default is six.
	var backoff int32

	deadline := int64(spec.EffectiveTimeout().Seconds())

	// The backstop for a process that died between creating this and deleting
	// it. Generous, because the ordinary path is the deferred delete in
	// RunTask and this only has to beat "forever".
	ttl := int32(3600)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.Name, Namespace: spec.Namespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       taskPodSpec(spec),
			},
		},
	}
}

// ensureTaskPullSecret writes the credential a task needs to pull its image.
//
// The same object an app's deploy maintains, under the same name, so a release
// running before its app's first apply creates it and the apply afterwards
// simply updates it to the same value. Two writers of one object is acceptable
// here because both write the identical credential from the same source; what
// would not be acceptable is a task inventing a second secret, which would
// leave the namespace holding two copies with no rule about which is current.
func (o *Orchestrator) ensureTaskPullSecret(
	ctx context.Context, spec orchestrator.TaskSpec,
) error {
	if len(spec.RegistryAuth) == 0 {
		return nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      orchestrator.PullSecretName,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
				orchestrator.LabelOwner:     string(spec.Owner),
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: spec.RegistryAuth},
	}

	secrets := o.client.CoreV1().Secrets(spec.Namespace)
	_, err := secrets.Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("k8s: store the pull credential for task %s: %w", spec.Ref, err)
	}
	return nil
}
