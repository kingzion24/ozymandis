package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

func init() {
	register(&command{
		name:    "resources",
		usage:   "resources set [--app N] [--cpu-request Q] [--cpu-limit Q] [--memory-request Q] [--memory-limit Q]",
		summary: "Set an app's CPU and memory request and limit",
		run:     runResources,
	})
}

func runResources(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New("oz: resources what? Try `oz resources set --cpu-limit 500m`")
	}
	return resourcesSet(ctx, env, args[1:])
}

// resourcesSet changes only the fields a flag was actually given for.
//
// A plain string flag cannot tell "not passed" from "passed as empty" apart —
// both read back as "" — and empty is a real, meaningful value here: it
// clears a field back to the namespace default. So this reads the app first
// and uses fs.Visit to find out which flags were actually typed, rather than
// which ones are merely non-empty. Without that, `oz resources set
// --cpu-limit 500m` would silently wipe out a memory limit set last week.
func resourcesSet(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz resources set", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	cpuRequest := fs.String("cpu-request", "", `cpu requested, e.g. "250m" — empty clears it`)
	cpuLimit := fs.String("cpu-limit", "", `cpu ceiling, e.g. "500m" — empty clears it`)
	memoryRequest := fs.String("memory-request", "", `memory requested, e.g. "256Mi" — empty clears it`)
	memoryLimit := fs.String("memory-limit", "", `memory ceiling, e.g. "512Mi" — empty clears it`)
	if err := fs.Parse(args); err != nil {
		return err
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	current, err := env.Client.App(ctx, name)
	if err != nil {
		return err
	}

	// Which flags were actually typed, not which ones are merely non-empty —
	// fs.Visit is what tells "not passed" apart from "passed as empty".
	changed := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cpu-request":
			changed[f.Name] = *cpuRequest
		case "cpu-limit":
			changed[f.Name] = *cpuLimit
		case "memory-request":
			changed[f.Name] = *memoryRequest
		case "memory-limit":
			changed[f.Name] = *memoryLimit
		}
	})
	if len(changed) == 0 {
		return errors.New("oz: nothing to change — pass at least one of " +
			"--cpu-request, --cpu-limit, --memory-request, --memory-limit")
	}
	next := mergeResourceFlags(current, changed)

	a, err := env.Client.SetResources(ctx, name,
		next.CPURequest, next.CPULimit, next.MemoryRequest, next.MemoryLimit)
	if err != nil {
		return err
	}

	if a.CPURequest == "" && a.CPULimit == "" && a.MemoryRequest == "" && a.MemoryLimit == "" {
		fmt.Fprintf(env.Err, "%s now runs on the namespace default.\n", a.Name)
		return nil
	}
	fmt.Fprintf(env.Err, "%s: cpu %s, memory %s\n",
		a.Name, requestLimitOf(a.CPURequest, a.CPULimit), requestLimitOf(a.MemoryRequest, a.MemoryLimit))
	return nil
}

// mergeResourceFlags applies only the fields present in changed onto current,
// leaving every other field exactly as it was.
//
// Pulled out of resourcesSet as its own function because it is the one piece
// of real logic in that command — the rest is flag parsing and an HTTP call —
// and it is what stands between "set one field" and "silently wipe out the
// other three."
func mergeResourceFlags(current App, changed map[string]string) App {
	next := current
	if v, ok := changed["cpu-request"]; ok {
		next.CPURequest = v
	}
	if v, ok := changed["cpu-limit"]; ok {
		next.CPULimit = v
	}
	if v, ok := changed["memory-request"]; ok {
		next.MemoryRequest = v
	}
	if v, ok := changed["memory-limit"]; ok {
		next.MemoryLimit = v
	}
	return next
}

// requestLimitOf renders a request/limit pair the way `oz status` and
// `oz resources set` both show it: "request/limit", either side blank when
// unset, or "default" when neither is set at all.
func requestLimitOf(request, limit string) string {
	if request == "" && limit == "" {
		return "default"
	}
	return orEmpty(request, "—") + "/" + orEmpty(limit, "—")
}

func orEmpty(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}
