package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"text/tabwriter"
)

func init() {
	register(&command{
		name:    "domains",
		usage:   "domains list|add HOST|remove HOST",
		summary: "Manage the hostnames an app answers on",
		run:     runDomains,
	})
}

// Domain mirrors the API's shape.
type Domain struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Verified bool   `json:"verified"`
	Target   string `json:"target"`
}

func (c *Client) Domains(ctx context.Context, name string) ([]Domain, string, error) {
	var out struct {
		Domains []Domain `json:"domains"`
		Managed string   `json:"managed"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+name+"/domains", nil, &out)
	return out.Domains, out.Managed, err
}

func (c *Client) AddDomain(ctx context.Context, name, host string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+name+"/domains",
		map[string]string{"host": host}, nil)
}

func (c *Client) RemoveDomain(ctx context.Context, name, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/apps/"+name+"/domains/"+id, nil, nil)
}

func runDomains(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("oz: domains what? Try `oz domains list`, " +
			"`oz domains add HOST`, or `oz domains remove HOST`")
	}
	switch args[0] {
	case "list", "ls":
		return domainsList(ctx, env, args[1:])
	case "add":
		return domainsAdd(ctx, env, args[1:])
	case "remove", "rm":
		return domainsRemove(ctx, env, args[1:])
	default:
		return fmt.Errorf("oz: no such domains command: %q", args[0])
	}
}

func domainsList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz domains list", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	domains, managed, err := env.Client.Domains(ctx, name)
	if err != nil {
		return domainsUnavailable(err)
	}

	if managed != "" {
		fmt.Fprintf(env.Err, "Platform hostname: %s\n\n", managed)
	}
	if len(domains) == 0 {
		fmt.Fprintf(env.Err, "%s has no custom domains.\n", name)
		return nil
	}

	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tVERIFIED\tCNAME TARGET")
	for _, d := range domains {
		verified := "no"
		if d.Verified {
			verified = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Host, verified, d.Target)
	}
	return tw.Flush()
}

func domainsAdd(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz domains add", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("oz: add which host? Try `oz domains add app.example.com`")
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	if err := env.Client.AddDomain(ctx, name, fs.Arg(0)); err != nil {
		return domainsUnavailable(err)
	}

	fmt.Fprintf(env.Err, "Claimed %s for %s.\n", fs.Arg(0), name)
	fmt.Fprintln(env.Err,
		"Nothing routes to it yet. Point a CNAME at the target in "+
			"`oz domains list`, then verify it in the dashboard.")
	return nil
}

func domainsRemove(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("oz domains remove", flag.ContinueOnError)
	appFlag := fs.String("app", "", "the app (default: [name] in ozymandis.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("oz: remove which host?")
	}

	name, err := appName(*appFlag)
	if err != nil {
		return err
	}

	// Resolved by host rather than taking an id, because a person knows the
	// hostname and would have to list first to find an id — and an id typed
	// from a previous listing is one that may since have been reused.
	domains, _, err := env.Client.Domains(ctx, name)
	if err != nil {
		return domainsUnavailable(err)
	}

	host := fs.Arg(0)
	for _, d := range domains {
		if d.Host == host {
			if err := env.Client.RemoveDomain(ctx, name, d.ID); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Removed %s from %s.\n", host, name)
			fmt.Fprintln(env.Err,
				"Traffic to it stops resolving, and its certificate goes with it.")
			return nil
		}
	}
	return fmt.Errorf("oz: %s does not answer on %q", name, host)
}

// domainsUnavailable turns the router's 404 into the real explanation.
//
// The domain endpoints are left off the router entirely on an install with no
// networking surface, so the failure arrives as "no such endpoint" — which is
// true and useless. This says which capability is missing instead.
func domainsUnavailable(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound &&
		apiErr.Code == "not_found" && apiErr.Message == "no such endpoint" {
		return errors.New(
			"oz: this install has no custom-domain support, so there are no " +
				"domains to manage. It needs OZYMANDIS_APP_DOMAIN configured.")
	}
	return err
}
