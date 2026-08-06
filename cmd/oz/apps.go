package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func init() {
	register(&command{
		name:    "apps",
		usage:   "apps",
		summary: "List the apps in this team",
		run:     runApps,
	})
	register(&command{
		name:    "status",
		usage:   "status [--app N]",
		summary: "Show one app's current state",
		run:     runStatus,
	})
	register(&command{
		name:    "scale",
		usage:   "scale N [--app N]",
		summary: "Set the replica count",
		run:     runScale,
	})
	register(&command{
		name:    "releases",
		usage:   "releases [--app N] [--limit N]",
		summary: "List recent deployments",
		run:     runReleases,
	})
	register(&command{
		name:    "config",
		usage:   "config [--app N] [--write]",
		summary: "Print the app's current config as ozymandis.toml",
		run:     runConfig,
	})
}

func runApps(ctx context.Context, env *Env, _ []string) error {
	apps, err := env.Client.Apps(ctx)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		fmt.Fprintf(env.Err, "No apps on %s yet.\n", env.ContextName)
		return nil
	}

	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tREPLICAS\tIMAGE\tURL")
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			a.Name, phaseOf(a), replicasOf(a), a.Image, a.URL())
	}
	return tw.Flush()
}

// phaseOf renders a status, distinguishing "not running" from "we could not
// ask" — the API leaves Status nil for the second, and collapsing them would
// have a script conclude an app is down when the cluster is merely unreachable.
func phaseOf(a App) string {
	if a.Status == nil {
		return "unknown"
	}
	return a.Status.Phase
}

func replicasOf(a App) string {
	if a.Status == nil {
		return fmt.Sprintf("?/%d", a.Replicas)
	}
	return fmt.Sprintf("%d/%d", a.Status.Ready, a.Status.Desired)
}

func runStatus(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz status", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	a, err := env.Client.App(ctx, name)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "app\t%s\n", a.Name)
	fmt.Fprintf(tw, "status\t%s\n", phaseOf(a))
	fmt.Fprintf(tw, "replicas\t%s\n", replicasOf(a))
	fmt.Fprintf(tw, "image\t%s\n", a.Image)
	fmt.Fprintf(tw, "source\t%s\n", a.Source)
	if a.Port > 0 {
		fmt.Fprintf(tw, "port\t%d\n", a.Port)
	}
	if u := a.URL(); u != "" {
		fmt.Fprintf(tw, "url\t%s\n", u)
	}
	if a.Status != nil && a.Status.Message != "" {
		fmt.Fprintf(tw, "message\t%s\n", a.Status.Message)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if a.Status == nil {
		fmt.Fprintln(env.Err,
			"\nThe cluster did not answer, so the status above is what was last "+
				"recorded rather than what is running.")
	}
	return nil
}

func runScale(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz scale", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("oz: scale to how many? Try `oz scale 3`")
	}

	n, err := strconv.Atoi(fs.Arg(0))
	if err != nil || n < 0 {
		return fmt.Errorf("oz: %q is not a replica count", fs.Arg(0))
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	a, err := env.Client.Scale(ctx, name, int32(n))
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Err, "Scaled %s to %d.\n", a.Name, a.Replicas)
	if n == 0 {
		fmt.Fprintln(env.Err, "It is now serving nothing.")
	}
	// Said because the file does not own this axis, and somebody who edits
	// [scale] afterwards expecting it to stick should hear it here.
	fmt.Fprintln(env.Err,
		"This is an operational change: a later `oz deploy` will leave it alone "+
			"unless you pass --scale.")
	return nil
}

func runReleases(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz releases", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	limit := fs.Int("limit", 10, "how many to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	deps, err := env.Client.Deployments(ctx, name, *limit)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		fmt.Fprintf(env.Err, "%s has never been deployed.\n", name)
		return nil
	}

	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tSTATUS\tRELEASE\tSOURCE\tIMAGE")
	for _, d := range deps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			d.StartedAt.Local().Format(time.RFC3339), d.Status,
			releaseLabel(d), d.Source, d.Image)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Said once, under the table, rather than in a column nobody can widen:
	// a failed release is the reason a deploy did not ship, and the log is
	// where the reason is.
	for _, d := range deps {
		if d.ReleaseStatus == "failed" && d.ReleaseLog != "" {
			fmt.Fprintf(env.Err, "\nThe release for %s failed:\n%s\n",
				d.StartedAt.Local().Format(time.RFC3339), indent(d.ReleaseLog))
			break
		}
	}
	return nil
}

// releaseLabel renders what the release did.
//
// "—" for a deployment older than the feature, which is different from
// "skipped": one means no release ran, the other means nobody can say.
func releaseLabel(d Deployment) string {
	if d.ReleaseStatus == "" {
		return "—"
	}
	return d.ReleaseStatus
}

// indent shifts a block of output so it reads as quoted rather than as this
// program's own words.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func runConfig(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz config", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	write := fs.Bool("write", false,
		"write ozymandis.toml here instead of printing it")
	force := fs.Bool("force", false, "with --write, overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	spec, err := env.Client.Config(ctx, name)
	if err != nil {
		return err
	}

	out, err := encodeSpec(spec)
	if err != nil {
		return err
	}

	if *write {
		return writeSpecFile(env, out, *force)
	}

	// Stdout, so the output can be piped. The note goes to stderr so it cannot
	// end up in whatever the output is redirected into.
	fmt.Fprint(env.Out, string(out))
	fmt.Fprintln(env.Err, configNote)
	return nil
}

const configNote = "\nNote: secrets are not in this file and cannot be — they " +
	"have no read path. Set them with `oz secrets set`."

// writeSpecFile writes ozymandis.toml in the working directory.
//
// This exists because the obvious spelling does not work:
//
//	oz config > ozymandis.toml
//
// The shell truncates ozymandis.toml to zero bytes BEFORE oz runs, and oz reads
// that same file to learn which app it is being asked about — so the redirect
// destroys its own input and the command fails with "name is required". It is
// the kind of bug that looks like the tool is broken rather than like the shell
// doing exactly what it was told.
//
// Naming the app explicitly (`oz config --app web > ozymandis.toml`) works, but
// requiring people to know why is worse than doing the write here, where the
// read happens first and the ordering is ours to control.
func writeSpecFile(env *Env, out []byte, force bool) error {
	const name = "ozymandis.toml"

	if _, err := os.Stat(name); err == nil && !force {
		return fmt.Errorf(
			"oz: %s already exists. Pass --force to overwrite it, or redirect "+
				"the output somewhere else:\n\n    oz config --app NAME > somewhere.toml",
			name)
	}

	if err := os.WriteFile(name, out, 0o644); err != nil {
		return fmt.Errorf("oz: write %s: %w", name, err)
	}

	fmt.Fprintf(env.Err, "Wrote %s.\n", name)
	fmt.Fprintln(env.Err, configNote)
	return nil
}
