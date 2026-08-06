// Command oz drives an Ozymandis install from a terminal or a CI job.
//
// Stdlib flag and a table of subcommands rather than a CLI framework. The
// engine has ten direct dependencies and a stated goal of being readable in an
// afternoon; a framework for a dozen commands would be the largest dependency
// in the tree and would earn it by generating help text.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
)

// command is one subcommand.
type command struct {
	name string

	// usage is the one-line form shown in help, without the leading "oz".
	usage string

	// summary is the sentence in the command list.
	summary string

	run func(ctx context.Context, env *Env, args []string) error
}

// Env is what every command needs: the resolved context and a client for it.
//
// Built once in main rather than by each command, so the precedence rules for
// --context live in one place and no command can accidentally read a different
// install than the one the person named.
type Env struct {
	// ContextName is the install being acted on, for messages that need to say
	// which one — a deploy going to the wrong install is the mistake this whole
	// context mechanism exists to make visible.
	ContextName string

	Client *Client

	// Out and Err are stdout and stderr. Fields rather than direct writes, so
	// tests can capture them and so the rule about which stream carries what is
	// enforceable: data on stdout, everything a person reads on stderr.
	Out *os.File
	Err *os.File
}

func main() {
	err := run()
	if err == nil {
		return
	}

	// A command that ran in a container and exited non-zero exits `oz` with the
	// same code, so a script sees what the command saw rather than a blanket 1.
	// The message is the command's own business — it already wrote whatever it
	// had to say to the terminal — so nothing is printed on top of it.
	if code, ok := exitCodeOf(err); ok {
		os.Exit(code)
	}

	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	// --context and --help are accepted before the subcommand as well as after,
	// because both readings are natural and refusing one is a papercut people
	// hit exactly once per install.
	var contextName string
	args, contextName = extractContext(args)

	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(os.Stdout)
		return nil
	}

	name := args[0]
	cmd, ok := commands[name]
	if !ok {
		usage(os.Stderr)
		return fmt.Errorf("oz: no command called %q", name)
	}

	// Interrupt cancels the context rather than killing the process, so a
	// `logs -f` closes its connection and a `deploy --watch` says what it was
	// waiting for instead of vanishing mid-line.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := &Env{Out: os.Stdout, Err: os.Stderr}

	// auth login is the one command that runs without a resolved context: it is
	// what creates one.
	if name != "auth" {
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		resolved, cc, err := cfg.Resolve(contextName)
		if err != nil {
			return err
		}
		env.ContextName = resolved
		env.Client = NewClient(cc)
	} else {
		env.ContextName = contextName
	}

	err := cmd.run(ctx, env, args[1:])

	// A cancelled context is somebody pressing ^C, which is not a failure worth
	// a stack of error text. Reported as a clean stop.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// extractContext pulls a --context flag out of args wherever it appears.
func extractContext(args []string) ([]string, string) {
	var out []string
	var name string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--context" || args[i] == "-context":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case len(args[i]) > 10 && args[i][:10] == "--context=":
			name = args[i][10:]
		default:
			out = append(out, args[i])
		}
	}
	return out, name
}

var commands = map[string]*command{}

func register(c *command) { commands[c.name] = c }

func usage(w *os.File) {
	fmt.Fprint(w, `oz — drive an Ozymandis install.

Usage:
    oz [--context NAME] <command> [flags]

Commands:
`)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, n := range names {
		fmt.Fprintf(tw, "    %s\t%s\n", commands[n].usage, commands[n].summary)
	}
	tw.Flush()

	fmt.Fprint(w, `
Run a command with --help for its flags.

The install acted on comes from --context, then OZ_CONTEXT, then the one
`+"`oz auth login`"+` last set. Configuration lives in ozymandis.toml beside your code.
`)
}

// appName resolves which app a command acts on.
//
// The flag wins, then the [name] in ozymandis.toml. A CLI that guessed from the
// directory name would deploy the wrong app in a monorepo, which is the case
// most likely to have several.
func appName(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	spec, err := loadSpec()
	if err != nil {
		return "", err
	}
	return spec.Name, nil
}
