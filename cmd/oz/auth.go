package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func init() {
	register(&command{
		name:    "auth",
		usage:   "auth login|whoami|logout",
		summary: "Manage credentials for an install",
		run:     runAuth,
	})
}

func runAuth(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("oz: auth what? Try `oz auth login`, `oz auth whoami`, or `oz auth logout`")
	}
	switch args[0] {
	case "login":
		return authLogin(ctx, env, args[1:])
	case "whoami":
		return authWhoami(ctx, env, args[1:])
	case "logout":
		return authLogout(ctx, env, args[1:])
	case "contexts":
		return authContexts(ctx, env)
	default:
		return fmt.Errorf("oz: no such auth command: %q", args[0])
	}
}

func authLogin(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz auth login", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "the install's URL, e.g. https://ozymandis.example")
	token := fs.String("token", "", "an API token from Settings › Access tokens")
	name := fs.String("name", "", "a name for this context (default: the endpoint's host)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *endpoint == "" || *token == "" {
		return errors.New(
			"oz: --endpoint and --token are both required.\n\n" +
				"    oz auth login --endpoint https://your-install --token oz_…\n\n" +
				"Create a token under Settings › Access tokens in the dashboard.")
	}

	u, err := url.Parse(*endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("oz: %q is not a URL", *endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("oz: %q needs an http:// or https:// scheme", *endpoint)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		// Said rather than refused. Plenty of installs are reached over a
		// tunnel or a private network, and the CLI is not in a position to know
		// — but a token crossing the open internet in the clear should not do
		// so silently.
		fmt.Fprintf(env.Err,
			"warning: %s is plain HTTP, so this token crosses the network "+
				"readable by anything on the path.\n", u.Host)
	}

	ctxName := *name
	if ctxName == "" {
		ctxName = u.Hostname()
	}

	// Verified before it is written. A CLI that stores a bad credential and
	// fails on the next command has reported the error in the wrong place, and
	// the person will believe the second command is what is broken.
	probe := NewClient(Context{Endpoint: *endpoint, Token: *token})
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	who, err := probe.Whoami(verifyCtx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			return errors.New("oz: that token was refused. Check it was copied whole — " +
				"it is shown once, when it is created.")
		}
		return err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.Contexts[ctxName] = Context{Endpoint: strings.TrimRight(*endpoint, "/"), Token: *token}
	cfg.Current = ctxName
	if err := cfg.Save(); err != nil {
		return err
	}

	path, _ := ConfigPath()
	fmt.Fprintf(env.Err, "Signed in to %s as %s (%s).\nContext %q saved to %s.\n",
		u.Host, teamLabel(who), who.Role, ctxName, path)
	return nil
}

func authWhoami(ctx context.Context, env *Env, _ []string) error {
	if env.Client == nil {
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		name, cc, err := cfg.Resolve(env.ContextName)
		if err != nil {
			return err
		}
		env.ContextName, env.Client = name, NewClient(cc)
	}

	who, err := env.Client.Whoami(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s\t%s\t%s\n", env.ContextName, teamLabel(who), who.Role)
	return nil
}

func authLogout(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz auth logout", flag.ContinueOnError)
	all := fs.Bool("all", false, "forget every context, not just the current one")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	if *all {
		cfg.Contexts = map[string]Context{}
		cfg.Current = ""
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintln(env.Err, "Forgot every context.")
		return nil
	}

	name, _, err := cfg.Resolve(env.ContextName)
	if err != nil {
		return err
	}
	delete(cfg.Contexts, name)
	if cfg.Current == name {
		cfg.Current = ""
		// Falling back to whatever remains rather than leaving no current
		// context, so logging out of one install does not make the CLI look
		// uninitialised when another is still configured.
		for _, remaining := range cfg.Names() {
			cfg.Current = remaining
			break
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(env.Err, "Forgot %q. The token is still live — revoke it in the "+
		"dashboard under Settings › Access tokens.\n", name)
	return nil
}

func authContexts(_ context.Context, env *Env) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Contexts) == 0 {
		return errNotLoggedIn
	}
	for _, n := range cfg.Names() {
		marker := " "
		if n == cfg.Current {
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %s\t%s\n", marker, n, cfg.Contexts[n].Endpoint)
	}
	return nil
}

func teamLabel(w Whoami) string {
	if w.TeamName != "" {
		return w.TeamName
	}
	return w.TeamID
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
