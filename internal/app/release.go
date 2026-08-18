package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// Release statuses, as stored on a deployment.
const (
	ReleaseSkipped     = "skipped"
	ReleaseSucceeded   = "succeeded"
	ReleaseFailed      = "failed"
	ReleaseUnavailable = "unavailable"
)

// releaseTimeout caps one release.
//
// Ten minutes: long enough for a migration that rewrites a table, short enough
// that a hung one fails the deploy rather than holding it until the deploy's own
// timeout. Well under DefaultTaskTimeout for the same reason — a release that
// runs for an hour has already gone wrong.
const releaseTimeout = 10 * time.Minute

// ErrReleaseFailed is a deploy vetoed by its release command.
//
// Its own error because the distinction matters to every caller: a failed build
// means nothing was produced, a failed apply means the cluster refused what was
// produced, and this means both worked and the app itself said do not ship
// this. Only the third is a statement by the code being deployed.
var ErrReleaseFailed = errors.New("app: the release command failed, so the deploy was stopped")

// ErrNoRunner means this install cannot run a release command.
var ErrNoRunner = errors.New(
	"app: this install cannot run a release command — its orchestrator cannot run tasks")

// SetReleaseCommand sets what runs against a new image before traffic moves.
//
// Not applied immediately, unlike SetCommand or SetHealth. A release command is
// not a property of the running workload — it is an instruction for the next
// deploy — so running it on save would execute a migration at the moment
// somebody typed it into a form.
func (s *Service) SetReleaseCommand(ctx context.Context, ownerID, name, command string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}

	command = strings.TrimSpace(command)
	if command != "" {
		// Parsed now so a bad command line is refused where it is typed, rather
		// than in the middle of a deploy that has already built an image.
		if _, err := ParseCommand(command); err != nil {
			return err
		}
	}

	if _, err := s.q.SetAppReleaseCommand(ctx, dbgen.SetAppReleaseCommandParams{
		OwnerID: ownerID, ID: a.ID, ReleaseCommand: command,
	}); err != nil {
		return fmt.Errorf("app: set release command: %w", err)
	}

	s.log.Info("release command set",
		slog.String("app", name), slog.Bool("cleared", command == ""))
	return nil
}

// noVolumesNote prefixes the log of a release for an app that has storage.
//
// The constraint is real and the failure it produces is confusing: a release
// that writes to /data gets "No such file or directory" and no hint why the
// path it can see in the running app is missing here. Three surfaces carry it —
// the doc comment on runRelease, a warning when a release command is configured
// on an app with volumes, and this line, which is the one somebody actually
// reads because it is sitting above the error.
const noVolumesNote = "release: this app's volumes are not mounted here — " +
	"a release runs beside the app, not in it\n"

// runRelease runs the release command against the new image, and reports
// whether the deploy may proceed.
//
// Between the build and the apply, which is the only useful place for it: after
// the image exists so it can run the code being deployed, and before traffic
// moves so a failure costs nothing. A release that ran after the apply would be
// a migration racing the pods that already need it.
//
// # Volumes are deliberately not mounted
//
// A release task attaching the app's PVCs would deadlock on a single-node
// install: local-path volumes are ReadWriteOnce and the running Deployment
// holds them, so the task pod stays Pending until the deploy times out — and
// the symptom is a deploy that hangs, which is far harder to read than one that
// fails. A release command talks to its database over the network, which is
// what it is for.
//
// # Failure is a veto
//
// A non-zero exit stops the deploy. Nothing is applied, so the previous image
// keeps serving: the whole point is that a migration which cannot run is caught
// before the code that assumes it did starts taking requests.
func (s *Service) runRelease(
	ctx context.Context, ownerID string, a App, deployID uuid.UUID,
) error {
	if strings.TrimSpace(a.ReleaseCommand) == "" {
		s.recordRelease(ctx, ownerID, deployID, ReleaseSkipped, "")
		return nil
	}

	runner, ok := s.orch.(orchestrator.Runner)
	if !ok {
		// Recorded and refused, not skipped. A release command that quietly
		// does not run is worse than no release command: somebody configured a
		// migration step and would have every reason to believe it ran.
		s.recordRelease(ctx, ownerID, deployID, ReleaseUnavailable, ErrNoRunner.Error())
		return ErrNoRunner
	}

	argv, err := ParseCommand(a.ReleaseCommand)
	if err != nil {
		s.recordRelease(ctx, ownerID, deployID, ReleaseFailed, err.Error())
		return err
	}

	plain, secrets, err := s.envFor(ctx, s.q, a.ID)
	if err != nil {
		s.recordRelease(ctx, ownerID, deployID, ReleaseFailed, err.Error())
		return err
	}

	// The note goes in whether or not the release touches storage, because the
	// only way to know it did is to read the failure it caused.
	var preamble string
	if len(a.Volumes) > 0 {
		preamble = noVolumesNote
	}

	auth, err := s.pullAuth(ctx, a)
	if err != nil {
		s.recordRelease(ctx, ownerID, deployID, ReleaseFailed, err.Error())
		return err
	}

	spec := orchestrator.TaskSpec{
		Ref: orchestrator.Ref{
			Owner:     orchestrator.OwnerID(ownerID),
			Namespace: a.Namespace,
			Name:      releaseTaskName(a.Name),
		},
		Image:   a.Image,
		Command: argv,
		Env:     plain,
		Secrets: secrets,

		// The app's own security posture, so the release runs as the same user
		// the app does. A migration that creates files the app cannot read is a
		// migration that succeeded and broke the app.
		RunAsUser: runtimeOf(a).RunAsUser,

		RegistryAuth: auth,
		Timeout:      releaseTimeout,
	}

	s.log.Info("running release command",
		slog.String("app", a.Name), slog.String("image", a.Image))

	result, runErr := runner.RunTask(ctx, spec)

	log := preamble + result.Output
	switch {
	case runErr != nil:
		// The task could not be run to completion — a timeout, or a cluster
		// that would not schedule it. Reported as a failure because the release
		// did not succeed, and a deploy must not proceed on "we could not tell".
		s.recordRelease(ctx, ownerID, deployID, ReleaseFailed, log+"\n"+runErr.Error())
		return fmt.Errorf("%w: %w", ErrReleaseFailed, runErr)

	case !result.Succeeded:
		s.recordRelease(ctx, ownerID, deployID, ReleaseFailed, log)
		return ErrReleaseFailed
	}

	s.recordRelease(ctx, ownerID, deployID, ReleaseSucceeded, log)
	s.log.Info("release command succeeded", slog.String("app", a.Name))
	return nil
}

// releaseTaskName is what the release's Job is called.
//
// Suffixed rather than named after the app, so a release cannot collide with
// the app's own workload or with a backup task for the same app.
func releaseTaskName(app string) string { return app + "-release" }

// recordRelease writes the outcome onto the deployment.
//
// Best effort and never returned: the release's own result is what the caller
// acts on, and failing to record a release that ran should not turn a good
// deploy into a failed one. The context is detached and bounded for the reason
// buildLogger's is — this is often called on the way out of a failure, where
// the caller's context may already be cancelled, and an unbounded write there
// would hang instead of losing a row.
func (s *Service) recordRelease(
	ctx context.Context, ownerID string, deployID uuid.UUID, status, log string,
) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := s.q.SetDeploymentRelease(writeCtx, dbgen.SetDeploymentReleaseParams{
		OwnerID: ownerID, ID: deployID, ReleaseStatus: status, ReleaseLog: log,
	}); err != nil {
		s.log.Error("record release outcome",
			slog.String("deployment", deployID.String()), slog.String("error", err.Error()))
	}
}
