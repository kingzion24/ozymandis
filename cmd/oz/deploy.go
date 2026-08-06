package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kingzion24/ozymandis/internal/appspec"
)

func init() {
	register(&command{
		name:    "deploy",
		usage:   "deploy [--app N] [--dry-run] [--scale] [--watch]",
		summary: "Converge ozymandis.toml and start a deployment",
		run:     runDeploy,
	})
}

// loadSpec reads ozymandis.toml from the working directory or above it.
//
// Walking up rather than requiring the exact directory, because `oz deploy`
// from a subdirectory of your own repository is what people type, and refusing
// it teaches nothing. It stops at a .git directory or the filesystem root, so
// it cannot wander into an unrelated project's config.
func loadSpec() (appspec.Spec, error) {
	dir, err := os.Getwd()
	if err != nil {
		return appspec.Spec{}, fmt.Errorf("oz: %w", err)
	}

	for {
		path := filepath.Join(dir, appspec.FileName)
		if _, err := os.Stat(path); err == nil {
			return appspec.Load(path)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return appspec.Spec{}, fmt.Errorf(
		"oz: no %s here or in any parent directory.\n\n"+
			"Create one, or name the app explicitly with --app.", appspec.FileName)
}

func runDeploy(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz deploy", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app to deploy (default: [name] in ozymandis.toml)")
	dryRun := fs.Bool("dry-run", false, "show what would change and stop")
	withScale := fs.Bool("scale", false, "also apply [scale] replicas from the file")
	watch := fs.Bool("watch", false, "wait for the deployment to finish, and exit non-zero if it fails")
	noConfig := fs.Bool("no-config", false, "skip the config converge and just redeploy")
	if err := fs.Parse(args); err != nil {
		return err
	}

	spec, specErr := loadSpec()
	name := *appFlag
	if name == "" {
		if specErr != nil {
			return specErr
		}
		name = spec.Name
	}

	// The converge, unless there is no file or it was waived.
	if specErr == nil && !*noConfig {
		result, err := env.Client.PutConfig(ctx, name, spec, *withScale, *dryRun)
		if err != nil {
			return err
		}
		reportChanges(env, result)

		if *dryRun {
			// Nothing was applied and nothing will be deployed. Said out loud,
			// because a dry run that printed a diff and exited silently reads
			// like a deploy that happened.
			fmt.Fprintln(env.Err, "\nDry run: nothing was changed and no deployment was started.")
			return nil
		}
	}

	if *dryRun {
		fmt.Fprintln(env.Err, "Dry run: no deployment was started.")
		return nil
	}

	dep, err := env.Client.Deploy(ctx, name)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return fmt.Errorf("oz: no app called %q on %s", name, env.ContextName)
		}
		return err
	}

	fmt.Fprintf(env.Err, "Deploying %s on %s…\n", name, env.ContextName)
	if !*watch {
		fmt.Fprintf(env.Err, "Started. Watch it with `oz status --app %s` "+
			"or re-run with --watch.\n", name)
		return nil
	}

	return watchDeployment(ctx, env, name, dep)
}

// reportChanges prints a converge to STDERR.
//
// Stderr, not stdout, for every line here: a diff is something a person reads,
// and `oz config show > ozymandis.toml` must not end up with a change log at
// the top of the file. Only data goes to stdout in this CLI.
//
// Skipped changes are printed on every run, not only when asked. A Skipped:true
// field in a JSON response nobody reads is the preview-lies problem moved up one
// level — the server was careful to report that it would not apply something,
// and a CLI that swallowed it would waste that care. Somebody who edits an
// image in the file and runs `oz deploy` has to be told the converge did not
// apply it, and told which verb does.
func reportChanges(env *Env, result ConfigResult) {
	var applied, deferred []Change
	for _, c := range result.Changes {
		if c.Skipped {
			deferred = append(deferred, c)
			continue
		}
		applied = append(applied, c)
	}

	if len(applied) > 0 {
		verb := "Changed"
		if result.DryRun {
			verb = "Would change"
		}
		fmt.Fprintf(env.Err, "%s:\n", verb)
		for _, c := range applied {
			fmt.Fprintf(env.Err, "  %s: %s → %s\n", c.Field, quoteEmpty(c.From), quoteEmpty(c.To))
			// A reason on an APPLIED change is a caveat rather than an excuse:
			// the change is happening and there is something about it worth
			// knowing — a release command on an app whose volumes it will not
			// see, for instance. Printed here because a reason the server took
			// the trouble to send and the CLI swallows is the same failure as a
			// skip nobody reads.
			if c.Reason != "" {
				fmt.Fprintf(env.Err, "      %s\n", c.Reason)
			}
		}
	}

	if len(deferred) > 0 {
		fmt.Fprintln(env.Err, "\nNot applied by this converge:")
		for _, c := range deferred {
			fmt.Fprintf(env.Err, "  %s: %s → %s\n", c.Field, quoteEmpty(c.From), quoteEmpty(c.To))
			if c.Reason != "" {
				fmt.Fprintf(env.Err, "      %s\n", c.Reason)
			}
		}
	}

	if len(result.UntrackedDomains) > 0 {
		// The file is not a faithful description of the app's routing, and
		// somebody reading only the file would never learn that.
		fmt.Fprintln(env.Err, "\nHostnames on this app that ozymandis.toml does not list:")
		for _, h := range result.UntrackedDomains {
			fmt.Fprintf(env.Err, "  %s\n", h)
		}
		fmt.Fprintln(env.Err, "  They are left alone. Remove one with `oz domains remove`.")
	}

	if len(applied) == 0 && len(deferred) == 0 {
		fmt.Fprintln(env.Err, "Configuration already matches ozymandis.toml.")
	}
}

func quoteEmpty(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// watchDeployment polls until the deployment ends.
//
// Polling rather than a stream, because a deployment's end is a row changing
// and there is no event to subscribe to. The interval is short enough to feel
// live and long enough that a CI job watching a ten-minute build does not make
// six hundred requests.
func watchDeployment(ctx context.Context, env *Env, name string, started Deployment) error {
	const interval = 2 * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Deliberately not a failure. The deploy is still running on the
			// server; the person just stopped watching it, and exiting non-zero
			// would fail a CI job for a deployment that may well succeed.
			fmt.Fprintf(env.Err, "\nStopped watching. %s is still deploying.\n", name)
			return ctx.Err()

		case <-ticker.C:
			deps, err := env.Client.Deployments(ctx, name, 5)
			if err != nil {
				return err
			}

			dep, ok := findDeployment(deps, started.ID)
			if !ok {
				// The id we started is not in the recent list. Rather than
				// waiting forever on a row that will never appear, take the
				// most recent one — which is what somebody watching means.
				if len(deps) == 0 {
					continue
				}
				dep = deps[0]
			}

			if !dep.Finished {
				continue
			}

			if dep.Status == "failed" {
				if dep.Message != "" {
					fmt.Fprintf(env.Err, "\n%s\n", dep.Message)
				}
				// Non-zero, which is the property that makes this usable in CI.
				return fmt.Errorf("oz: deploying %s failed", name)
			}

			fmt.Fprintf(env.Err, "Deployed %s.\n", name)
			if app, err := env.Client.App(ctx, name); err == nil {
				if url := app.URL(); url != "" {
					fmt.Fprintf(env.Err, "  %s\n", url)
				}
			}
			return nil
		}
	}
}

func findDeployment(deps []Deployment, id string) (Deployment, bool) {
	for _, d := range deps {
		if d.ID == id {
			return d, true
		}
	}
	return Deployment{}, false
}

// writeAll is io.Writer plumbing used by the log commands.
var _ = io.Discard
