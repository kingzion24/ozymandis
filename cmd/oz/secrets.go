package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/kingzion24/ozymandis/internal/appspec"
)

func init() {
	register(&command{
		name:    "secrets",
		usage:   "secrets list|set K=V…|unset K",
		summary: "Manage an app's environment",
		run:     runSecrets,
	})
	register(&command{
		name:    "logs",
		usage:   "logs [--app N] [-f] [-n N]",
		summary: "Read an app's output",
		run:     runLogs,
	})
}

func runSecrets(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("oz: secrets what? Try `oz secrets list`, " +
			"`oz secrets set KEY=value`, or `oz secrets unset KEY`")
	}
	switch args[0] {
	case "list", "ls":
		return secretsList(ctx, env, args[1:])
	case "set":
		return secretsSet(ctx, env, args[1:])
	case "unset", "rm":
		return secretsUnset(ctx, env, args[1:])
	default:
		return fmt.Errorf("oz: no such secrets command: %q", args[0])
	}
}

func secretsList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz secrets list", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	vars, err := env.Client.Variables(ctx, name)
	if err != nil {
		return err
	}
	if len(vars) == 0 {
		fmt.Fprintf(env.Err, "%s has no environment set.\n", name)
		return nil
	}

	sort.Slice(vars, func(i, j int) bool { return vars[i].Key < vars[j].Key })

	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE")
	for _, v := range vars {
		value := v.Value
		if v.Secret {
			// Not a redaction this CLI performs: the server does not send the
			// value, because a sealed value has no read path anywhere. Shown as
			// a marker rather than blank so the row does not read as an empty
			// variable.
			value = "(sealed)"
		}
		fmt.Fprintf(tw, "%s\t%s\n", v.Key, value)
	}
	return tw.Flush()
}

func secretsSet(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz secrets set", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	plain := fs.Bool("plain", false,
		"store readable rather than sealed — for a log level, not a password")
	stdin := fs.Bool("stdin", false, "read KEY=value lines from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	vars := map[string]string{}

	if *stdin {
		// So a value can come from a file or a password manager without ever
		// being an argument. Command lines are visible in `ps` output and land
		// in shell history; a credential passed that way is a credential
		// somebody else on the machine can read.
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return fmt.Errorf("oz: %q on stdin is not KEY=value", line)
			}
			vars[strings.TrimSpace(k)] = v
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("oz: read stdin: %w", err)
		}
	}

	for _, arg := range fs.Args() {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("oz: %q is not KEY=value", arg)
		}
		vars[strings.TrimSpace(k)] = v
	}

	if len(vars) == 0 {
		return errors.New("oz: nothing to set. Try `oz secrets set KEY=value`")
	}

	if err := env.Client.SetVariables(ctx, name, vars, !*plain); err != nil {
		return err
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	kind := "Sealed"
	if *plain {
		kind = "Set"
	}
	// The keys, never the values. This process has them in memory and printing
	// them back would put them in the terminal scrollback and the CI log of
	// whatever ran the command.
	fmt.Fprintf(env.Err, "%s %s on %s.\n", kind, strings.Join(keys, ", "), name)
	fmt.Fprintln(env.Err, "Deploy to pick them up: `oz deploy`.")
	return nil
}

func secretsUnset(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz secrets unset", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("oz: unset which key?")
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	for _, key := range fs.Args() {
		if err := env.Client.DeleteVariable(ctx, name, key); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.NotFound() {
				fmt.Fprintf(env.Err, "%s: no such variable.\n", key)
				continue
			}
			return err
		}
		fmt.Fprintf(env.Err, "Unset %s on %s.\n", key, name)
	}
	return nil
}

func runLogs(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz logs", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	follow := fs.Bool("f", false, "keep the connection open and print lines as they arrive")
	tail := fs.Int("n", 200, "how many lines of history to show first")
	if err := fs.Parse(args); err != nil {
		return err
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	if !*follow {
		lines, err := env.Client.Logs(ctx, name, *tail)
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Fprintln(env.Out, l.Text)
		}
		return nil
	}

	body, err := env.Client.LogStream(ctx, name, *tail)
	if err != nil {
		return err
	}
	defer body.Close()

	// NDJSON: one object per line, decoded as it arrives. A decoder rather than
	// a scanner because a log line containing a newline would break a
	// line-oriented reader, and application logs contain stack traces.
	dec := json.NewDecoder(body)
	for {
		var line LogLine
		if err := dec.Decode(&line); err != nil {
			// The context being done is somebody pressing ^C, which main
			// reports as a clean stop rather than a failure.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// EOF is the server closing the stream — the pod went away, or the
			// connection dropped. Not an error worth a non-zero exit for a
			// command whose whole job is to watch until something ends.
			return nil
		}
		if line.Error != "" {
			// Reported in-band by the server, because the status was sent long
			// before this went wrong.
			fmt.Fprintf(env.Err, "\nstream ended: %s\n", line.Error)
			return fmt.Errorf("oz: the log stream failed")
		}
		fmt.Fprintln(env.Out, line.Text)
	}
}

// encodeSpec renders the API's spec as TOML.
//
// It decodes into appspec.Spec first, rather than encoding the raw JSON map,
// and the reason is types. JSON has one number type: every integer arrives as a
// float64, and encoding that map directly writes `replicas = 2.0` and
// `port = 8080.0` — which the TOML decoder then refuses on the way back in,
// because the spec's fields are integers. The output of `oz config --write`
// would not parse with `oz deploy`, which is the one property that command
// exists to have.
//
// The trade is that a field a newer server knows about and this build does not
// would be dropped. That is caught rather than accepted: the decode refuses
// unknown fields and says to upgrade, because appspec.Parse refuses them too —
// so a file carrying one could not be deployed by this CLI regardless, and
// failing at the write with an explanation beats failing at the deploy without.
func encodeSpec(raw map[string]any) ([]byte, error) {
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("oz: re-encode the server's reply: %w", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()

	var spec appspec.Spec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf(
			"oz: this install's config has something this version of oz does not "+
				"understand (%w).\nUpgrade oz, or fetch it as JSON with "+
				"`curl -H \"Authorization: Bearer …\" <endpoint>/api/v1/apps/NAME/config`",
			err)
	}

	out, err := appspec.Encode(spec)
	if err != nil {
		return nil, err
	}
	return out, nil
}
