// Command ozymandis runs the Ozymandis engine: a self-hosted PaaS control plane for
// Kubernetes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kingzion24/ozymandis/internal/account"
	"github.com/kingzion24/ozymandis/internal/api"
	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/cluster"
	"github.com/kingzion24/ozymandis/internal/config"
	"github.com/kingzion24/ozymandis/internal/domain"
	"github.com/kingzion24/ozymandis/internal/identity"
	"github.com/kingzion24/ozymandis/internal/notify"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
	"github.com/kingzion24/ozymandis/internal/orchestrator/k8s"
	"github.com/kingzion24/ozymandis/internal/registry"
	"github.com/kingzion24/ozymandis/internal/secret"
	"github.com/kingzion24/ozymandis/internal/store"
	"github.com/kingzion24/ozymandis/internal/web"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/ozymandis
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ozymandis: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.Debug)
	log.Info("starting ozymandis",
		slog.String("version", version),
		slog.String("config", cfg.String()),
	)

	// Signals cancel the root context, which unwinds startup and serving
	// alike — so a Ctrl-C during a slow cluster connect exits promptly
	// instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
		return err
	}

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	orch, err := newOrchestrator(ctx, cfg, log)
	if err != nil {
		return err
	}

	// Non-nil only when sign-in is switched on. Held here rather than inside
	// newIdentity because the dashboard needs the same service the session
	// provider resolves against — one that signed a cookie the other did not
	// know about would be a sign-in that never takes.
	var accounts *account.Service
	var mailer notify.Mailer
	if cfg.AccountsEnabled() {
		accounts = account.NewService(pool, log)

		// Built before serving starts so that a relay which could never work
		// stops startup, rather than surfacing at the first sign-in attempt
		// where it reads to an operator as sign-in being broken.
		if mailer, err = newMailer(cfg, log); err != nil {
			return err
		}

		// The one account that exists before anybody creates one.
		//
		// Seeded at every startup and idempotent: the upsert behind it does not
		// touch an existing password, so restarting the process cannot put the
		// built-in default back over one somebody has changed. Without this a
		// fresh install has a sign-in form and nobody to type into it.
		if err := seedSuperuser(ctx, cfg, accounts, log); err != nil {
			return err
		}
	}

	ident, err := newIdentity(cfg, accounts, log)
	if err != nil {
		return err
	}

	// Built before serving so a malformed key stops startup rather than
	// surfacing the first time somebody saves a secret — by which point they
	// read it as the feature being broken.
	var keeper *secret.Keeper
	if cfg.SecretKey != "" {
		if keeper, err = secret.NewKeeper(cfg.SecretKey); err != nil {
			return err
		}
	} else {
		log.Warn("no OZYMANDIS_SECRET_KEY set — environment variables can be stored, " +
			"but marking one secret will be refused rather than stored readable; " +
			"generate a key with `openssl rand -base64 32`")
	}

	// Where built images go, and what runs the build. Both optional: without a
	// key there is no registry, and an orchestrator that cannot run a Job is a
	// working orchestrator — the source is listed as unavailable rather than
	// offered and failed.
	var images app.Images
	var builder app.Builder
	if keeper.Configured() {
		images = registry.New(pool, keeper, log)
		if b, ok := orch.(orchestrator.Builder); ok {
			builder = b
		}
	}

	apps := app.NewService(pool, orch, log, app.Options{
		Builder:         builder,
		Images:          images,
		AppDomain:       cfg.AppDomain,
		CertResolver:    orchestrator.IssuerRef{Name: cfg.CertResolver},
		Keeper:          keeper,
		ReservedDomains: cfg.ReservedDomains,
		// The standard resolver. Verifying a custom domain is a DNS lookup,
		// and an install that cannot make one says so rather than failing in a
		// way that looks like the domain being wrong.
		Resolver: domain.NetResolver{},
	})

	if cfg.AppDomain != "" {
		log.Info("per-app hostnames enabled",
			slog.String("app_domain", cfg.AppDomain),
			slog.String("dns", "point *."+cfg.AppDomain+" at this cluster"))
	}

	// Said at startup because neither branch is verifiable from here and both
	// land on somebody who is not reading this log.
	//
	// With a resolver named, Ozymandis cannot check the controller actually has
	// one by that name — a wrong name serves the controller's own certificate
	// to every visitor while every deploy stays green. With none, hostnames are
	// verified, routed, and then served over plain HTTP. Printing the resolver
	// is what makes the first case checkable against the controller's config.
	if cfg.CertResolver != "" {
		log.Info("hostnames get their own certificates",
			slog.String("acme_resolver", cfg.CertResolver),
			slog.String("verify", "the resolver must exist in the ingress controller's configuration"))
	} else {
		log.Info("no ACME resolver configured — hostnames are served over plain HTTP; " +
			"set OZYMANDIS_CERT_RESOLVER to change that")
	}

	if err := apps.EnsureOwner(ctx, cfg.OwnerID, cfg.OwnerName, ""); err != nil {
		return err
	}

	opts := web.Options{
		Orchestrator: orch,
		Identity:     ident,
		Apps:         apps,
		// Slots is left nil: the engine uses its own single-owner chrome.
		// An application wrapping the engine passes its own SlotProvider
		// here, which is the whole of what it takes to add tenant chrome.
		// Accounts are a credential of their own, so the settings page must not
		// report the install as open to anyone merely because no shared token
		// is set.
		Authenticated: cfg.AccountsEnabled() || !cfg.Unauthenticated(),
		Version:       version,
		AppDomain:     cfg.AppDomain,
		CertResolver:  cfg.CertResolver,
		Logger:        log,
		Nets:          apps,
		Logs:          apps,
	}

	// The add-node surface only exists where a token could actually be sealed.
	// Without a key the page could store nothing and hand out nothing, so it is
	// left off the router entirely rather than shown and refused.
	if keeper.Configured() {
		opts.Joiner = cluster.New(pool, keeper, log)
		// Every stack mints credentials, so the same key that gates joining
		// gates this. Offered without one it would fail after creating the
		// first app.
		opts.Stacks = apps
		// And the registry, for the third time the same reason: it holds a
		// push credential, and a credential this install cannot seal is one it
		// declines to hold.
		opts.Registries = registry.New(pool, keeper, log)

		// Deploy on push, for the fifth time the same reason: the webhook
		// secret and the deploy key are both credentials, and an install that
		// cannot seal them declines to hold them rather than storing them
		// readable.
		opts.Pushes = apps

		// Backups, and the same reason a fourth time with more at stake: the
		// destination holds both a storage credential and the password that
		// encrypts every snapshot. An install that cannot seal those would be
		// storing the key to its backups readable in the database those backups
		// are of.
		//
		// Also requires an orchestrator that can run Jobs. Asserted here rather
		// than assumed, so an orchestrator without that support leaves the
		// surface off instead of offering a schedule that would never fire.
		if _, ok := orch.(orchestrator.Runner); ok {
			opts.Backups = apps
		} else {
			log.Info("backups are off — this orchestrator cannot run scheduled jobs")
		}
	} else {
		log.Info("add-node, the image registry and backups are off — " +
			"set OZYMANDIS_SECRET_KEY to store a cluster join token, registry " +
			"password or backup credentials")
	}

	if cfg.AccountsEnabled() {
		opts.Accounts = accounts
		opts.Mailer = mailer
		opts.BaseURL = cfg.BaseURL
		opts.SessionTTL = cfg.SessionTTL
		// The team the install has been running as. The first person to sign in
		// inherits it, so the apps already deployed under OZYMANDIS_OWNER_ID stay
		// reachable instead of belonging to an owner nobody can authenticate as.
		opts.BootstrapTeamID = cfg.OwnerID
		opts.BootstrapTeamName = cfg.OwnerName
		opts.MailTransport = cfg.MailTransport()
	}

	srv, err := web.New(opts)
	if err != nil {
		return err
	}

	handler, err := mount(srv, ident, apps, accounts, cfg, log)
	if err != nil {
		return err
	}

	// Settles builds whose process went away — a restart mid-build, or a
	// replica that stopped. Level-triggered against the Job rather than driven
	// by anything this process remembers, so it is correct after a restart and
	// correct when several replicas run it at once.
	go apps.RunReconciler(ctx)

	// Deploy-on-push for installs GitHub cannot reach. The webhook is the
	// primary path and this is the fallback; an install with a working hook
	// simply finds nothing new on every pass.
	go apps.RunPoller(ctx)

	return serve(ctx, cfg, handler, log)
}

// mount puts the JSON API alongside the dashboard.
//
// A root mux rather than the API being routes inside web.Server. The two are
// different surfaces for different callers — one renders pages and redirects to
// a sign-in form, the other returns status codes and never redirects anywhere —
// and mounting them separately is what keeps the dashboard's browser-shaped
// middleware from ever running on an API request.
//
// The pattern is "/api/" rather than "/api/v1/": ServeMux matches the longest
// pattern, so this claims every future version too, and an unrecognised one
// gets the API's JSON 404 instead of the dashboard's HTML.
func mount(
	srv *web.Server, ident identity.Provider, apps *app.Service,
	accounts *account.Service, cfg config.Config, log *slog.Logger,
) (http.Handler, error) {
	opts := api.Options{
		Identity: ident,
		Apps:     apps,
		Logs:     apps,
		Nets:     apps,
		Exec:     apps,
		Logger:   log,
	}

	// Roles only exist where accounts do. Left nil, every authenticated caller
	// acts as the owner — which is the literal truth of a shared-token install
	// rather than a permissive default, and is what web.roleOf concludes for
	// the same install.
	if cfg.AccountsEnabled() {
		opts.Roles = accounts.RequestRoles(web.SessionCookie)
	}

	apiSrv, err := api.New(opts)
	if err != nil {
		return nil, err
	}

	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())

	// Outside the identity middleware entirely: GitHub carries no credential of
	// ours, and the delivery's signature is the authentication. Mounted here
	// rather than inside the API router so it cannot inherit an auth middleware
	// by accident — a webhook behind a bearer-token gate is a webhook that
	// never fires.
	root.Handle("/webhooks/", api.WebhookHandler(apps, log))

	root.Handle("/", srv.Handler())
	return root, nil
}

// newOrchestrator connects to a cluster, or falls back to an in-memory stub.
//
// Falling back rather than exiting is deliberate: a self-hoster should be able
// to start the dashboard, see a clear "cluster unreachable" state, and fix
// their kubeconfig from there — rather than face a process that refuses to
// boot and a log line they have to find.
func newOrchestrator(
	ctx context.Context, cfg config.Config, log *slog.Logger,
) (orchestrator.Orchestrator, error) {
	orch, err := k8s.New(ctx, k8s.Config{
		InCluster:  cfg.KubeInCluster,
		Kubeconfig: cfg.Kubeconfig,
	}, log)
	if err == nil {
		log.Info("connected to cluster")
		return orch, nil
	}

	log.Warn("cluster unreachable — starting with an in-memory orchestrator; "+
		"deploys will not reach a cluster until this is fixed",
		slog.String("error", err.Error()),
	)
	return orchestrator.NewNoop(), nil
}

// newIdentity picks which of the providers resolves an owner.
//
// Which one is active decides who can reach the dashboard and what they see, so
// each branch says so in the log: an operator must be able to tell from a
// startup line whether the install signs people in, guards a shared token, or
// trusts everyone who can reach the port.
// seedSuperuser creates the built-in administrator and gives them the team the
// install runs as.
//
// Two steps, because they answer different questions: EnsureSuperuser decides
// who may sign in, BootstrapOwner decides what they own once they have. A
// superuser with no membership resolves to no owner and meets a dashboard they
// cannot use, which looks exactly like a broken install.
//
// The warning is not decoration. The default password is a constant in a public
// repository, so an install still running it can be signed into by anyone who
// has read the source — and the only place that fact can reach the operator is
// the log they are already watching at startup.
func seedSuperuser(
	ctx context.Context, cfg config.Config, accounts *account.Service, log *slog.Logger,
) error {
	user, err := accounts.EnsureSuperuser(ctx, cfg.SuperuserName(), cfg.SuperuserPassword())
	if err != nil {
		return fmt.Errorf("seed the superuser: %w", err)
	}
	if err := accounts.BootstrapOwner(ctx, cfg.OwnerID, cfg.OwnerName, user); err != nil {
		return fmt.Errorf("give the superuser an owner to act as: %w", err)
	}

	if cfg.UsingDefaultSuperuserPassword() {
		log.Warn("the superuser is using the built-in default password — "+
			"anybody who has read the source can sign in. Change it on the team "+
			"page, or set OZYMANDIS_SUPERUSER_PASSWORD",
			slog.String("username", user.Username))
	} else {
		log.Info("superuser ready", slog.String("username", user.Username))
	}
	return nil
}

func newIdentity(
	cfg config.Config, accounts *account.Service, log *slog.Logger,
) (identity.Provider, error) {
	if cfg.AccountsEnabled() {
		log.Info("accounts enabled — requests are resolved from an API token or a "+
			"session cookie to the team it is acting as",
			slog.String("base_url", cfg.BaseURL),
			slog.String("mail_transport", cfg.MailTransport()),
			slog.Duration("session_ttl", cfg.SessionTTL),
		)
		// No warning about the mail transport any more. Sign-in used to be a
		// link, so an install with nowhere to send one was an install nobody
		// could enter, and saying so at startup was the difference between that
		// and waiting for mail forever. A password needs no transport at all, so
		// an install with none is complete rather than half-configured.
		// Tokens ahead of sessions. Both resolve to a team and everything
		// downstream takes the result without asking which arrived, so this is
		// the only place the two ways in have to be reconciled — and a request
		// that carries both should be judged on the one it sent deliberately.
		return identity.First(
			accounts.TokenProvider(),
			accounts.Provider(web.SessionCookie),
		), nil
	}

	owner := identity.Owner{ID: cfg.OwnerID, DisplayName: cfg.OwnerName}

	if cfg.Unauthenticated() {
		log.Warn("no OZYMANDIS_AUTH_TOKEN set — the dashboard is unauthenticated. " +
			"Only run this way on a trusted network.")
		return identity.NewSingleOwner(owner), nil
	}
	log.Info("shared-token authentication — every caller acts as the single owner",
		slog.String("owner", cfg.OwnerID))
	return identity.NewStaticToken(owner, cfg.AuthToken)
}

// newMailer builds the transport the install sends mail through.
//
// The logging transport is the fallback rather than an error, and no longer
// warns about it. It used to carry sign-in links, so an install with no relay
// was one whose credentials sat in a log file — worth saying loudly. Sign-in is
// a password now and sends nothing, so an install with no transport is complete
// rather than half-configured.
func newMailer(cfg config.Config, log *slog.Logger) (notify.Mailer, error) {
	switch {
	case cfg.SMTPAddr != "":
		return notify.NewSMTP(notify.SMTPConfig{
			Addr:     cfg.SMTPAddr,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
		})
	case cfg.ResendAPIKey != "":
		return notify.NewResend(cfg.ResendAPIKey, cfg.ResendFrom)
	default:
		return notify.NewLog(log), nil
	}
}

func serve(
	ctx context.Context, cfg config.Config, handler http.Handler, log *slog.Logger,
) error {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
