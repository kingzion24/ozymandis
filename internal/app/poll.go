package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// PollInterval is how often the poller looks for new commits.
//
// Five minutes: often enough that somebody who cannot receive webhooks is not
// waiting long, and rare enough that a pod per pass is nothing. It is the
// fallback rather than the primary path — an install GitHub can reach uses the
// webhook and never notices this.
const PollInterval = 5 * time.Minute

// pollImage is the git client a poll runs in.
//
// The same image the build's clone step uses, named here rather than imported
// from the k8s package: this layer must not depend on a specific orchestrator,
// and an image name is a string.
const pollImage = "alpine/git:latest"

// pollTimeout caps one pass.
//
// A pass that hangs holds nothing but itself, but an unbounded one accumulates:
// the ticker keeps firing and each stuck pass leaves a Job behind.
const pollTimeout = 2 * time.Minute

// RunPoller checks auto-deploy repositories for new commits until ctx is done.
//
// For installs GitHub cannot reach — behind NAT, on a private network, or
// simply not configured with a webhook. A person who cannot receive a delivery
// should still get deploy-on-push, a few minutes later.
//
// # One task per pass, not one per app
//
// Every repository is checked by a SINGLE Job that runs git ls-remote over all
// of them and prints one line each. A pod per app per five minutes would be
// twelve pods an hour for one app and rather a lot for twenty, on a cluster
// whose whole selling point is that it fits on a small machine.
//
// The control plane does not run git itself, deliberately. It has no git binary
// and no business gaining one — and the credential a private repository needs
// is a deploy key that already has a path into a build pod. Reusing that path
// means one way to authenticate a clone rather than two.
func (s *Service) RunPoller(ctx context.Context) {
	if _, ok := s.orch.(orchestrator.Runner); !ok {
		s.log.Info("polling for pushes is off — this orchestrator cannot run tasks; " +
			"webhooks still work")
		return
	}

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.pollOnce(ctx); err != nil {
				// Logged, never fatal. A poll that fails is a poll; the next one
				// is five minutes away and the webhook path is unaffected.
				s.log.Warn("poll for pushes failed", slog.String("error", err.Error()))
			}
		}
	}
}

// pollOnce runs one pass.
func (s *Service) pollOnce(ctx context.Context) error {
	rows, err := s.q.ListAutoDeployApps(ctx)
	if err != nil {
		return fmt.Errorf("app: list auto-deploy apps: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	runner, ok := s.orch.(orchestrator.Runner)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	// Grouped by namespace, because a task runs in one. In practice every
	// auto-deploy app of one team shares a namespace, so this is usually a
	// single task per team.
	byNamespace := map[string][]App{}
	for _, row := range rows {
		a := toApp(row)
		if !a.Repo.Set() {
			continue
		}
		byNamespace[a.Namespace] = append(byNamespace[a.Namespace], a)
	}

	for namespace, apps := range byNamespace {
		heads, err := s.lsRemote(ctx, runner, namespace, apps)
		if err != nil {
			s.log.Warn("could not read remote heads",
				slog.String("namespace", namespace), slog.String("error", err.Error()))
			continue
		}
		for _, a := range apps {
			sha, ok := heads[a.Name]
			if !ok || sha == "" || sha == a.LastDeployedSHA {
				continue
			}
			s.deployFromPoll(ctx, a, sha)
		}
	}
	return nil
}

// lsRemote asks one Job for every repository's current head.
//
// The output is one "app<TAB>sha" line per repository. Parsed rather than
// trusted: a repository that cannot be reached prints nothing for itself and
// the others still report, so one unreachable host does not stop the pass.
func (s *Service) lsRemote(
	ctx context.Context, runner orchestrator.Runner, namespace string, apps []App,
) (map[string]string, error) {
	var script strings.Builder
	script.WriteString("set -u\n")
	for _, a := range apps {
		// The name and ref are passed through the environment rather than
		// interpolated into the script, the same rule cloneStep follows: a
		// repository URL is somebody's input and a value spliced into a shell
		// command is a command they get to extend.
		script.WriteString(fmt.Sprintf(
			"sha=$(git ls-remote \"$REPO_%s\" \"refs/heads/$REF_%s\" 2>/dev/null "+
				"| head -n1 | cut -f1) || sha=\"\"\n"+
				"printf '%%s\\t%%s\\n' \"$NAME_%s\" \"$sha\"\n",
			envKey(a.Name), envKey(a.Name), envKey(a.Name)))
	}

	env := map[string]string{}
	for _, a := range apps {
		k := envKey(a.Name)
		env["REPO_"+k] = a.Repo.URL
		env["REF_"+k] = a.Repo.Ref()
		env["NAME_"+k] = a.Name
	}

	result, err := runner.RunTask(ctx, orchestrator.TaskSpec{
		Ref: orchestrator.Ref{
			Owner:     orchestrator.OwnerID(apps[0].OwnerID),
			Namespace: namespace,
			Name:      "poll-remotes",
		},
		Image:   pollImage,
		Command: []string{"/bin/sh", "-euc", script.String()},
		Env:     env,
		Timeout: pollTimeout,
	})
	if err != nil {
		return nil, err
	}
	return parseLsRemote(result.Output), nil
}

// parseLsRemote reads the "name<TAB>sha" lines a poll task prints.
func parseLsRemote(out string) map[string]string {
	heads := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, sha, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name == "" || sha == "" {
			continue
		}
		heads[name] = sha
	}
	return heads
}

// envKey turns an app name into something legal in a shell variable name.
func envKey(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// deployFromPoll starts a deploy for a commit the poll found.
//
// The poll has NO path information — ls-remote reports a head, not a diff — so
// a monorepo app deploys on any new commit to its branch. Said in the log
// rather than silently: somebody comparing webhook behaviour with poll
// behaviour would otherwise find them inconsistent with no explanation.
func (s *Service) deployFromPoll(ctx context.Context, a App, sha string) {
	if err := s.q.SetAppLastDeployedSHA(ctx, dbgen.SetAppLastDeployedSHAParams{
		OwnerID: a.OwnerID, ID: a.ID, LastDeployedSha: sha,
	}); err != nil {
		s.log.Error("record the polled commit",
			slog.String("app", a.Name), slog.String("error", err.Error()))
		return
	}

	if err := s.Redeploy(ctx, a.OwnerID, a.Name); err != nil {
		s.log.Warn("deploy from poll failed",
			slog.String("app", a.Name), slog.String("error", err.Error()))
		return
	}

	msg := "deployed from a poll"
	if a.Repo.Subdir != "" {
		msg += " — a poll sees only the head, not which files changed, " +
			"so this deploys on any new commit to " + a.Repo.Ref()
	}
	s.log.Info(msg,
		slog.String("app", a.Name), slog.String("commit", sha))
}
