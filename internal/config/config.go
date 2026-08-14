// Package config loads runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/kingzion24/ozymandis/internal/account"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var appDomainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// k8sName matches an RFC 1123 label, which is what a Kubernetes object name
// has to be. Used for values that are written into an annotation and only
// validated by another controller, where a typo is silent.
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// DNS limits, and the app-name limit they have to accommodate.
//
// maxAppName mirrors the 40-character cap app.CreateInput.Validate enforces.
// It is duplicated rather than imported because config sits below app in the
// dependency order; if the two ever disagree, this one is the conservative
// side and fails at startup rather than per-app.
const (
	maxHostname = 253
	maxLabel    = 63
	maxAppName  = 40
)

// domainFault returns why a domain is unusable, or "" if it is fine.
func domainFault(d string) string {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	switch {
	case d == "":
		return "must not be empty"
	case !appDomainRE.MatchString(d):
		return "must be a valid dotted domain name"
	}
	for _, label := range strings.Split(d, ".") {
		if len(label) > maxLabel {
			return fmt.Sprintf("label %q exceeds %d characters", label, maxLabel)
		}
	}
	return ""
}

// Config is everything the engine needs to start.
type Config struct {
	// Addr is the listen address for the dashboard.
	Addr string

	// DatabaseURL is a Postgres connection string. Required.
	DatabaseURL string

	// Kubeconfig points at a cluster. Empty uses the default client-go
	// loading rules, or in-cluster config when KubeInCluster is set.
	Kubeconfig    string
	KubeInCluster bool

	// AuthToken, when set, requires a bearer token on every request. Empty
	// means the dashboard is unauthenticated and suitable only for a trusted
	// network — the engine warns loudly about this at startup.
	AuthToken string

	// OwnerID identifies the single owner every resource belongs to.
	OwnerID   string
	OwnerName string

	// SecretKey seals secret environment variables, base64 of 32 bytes.
	//
	// Empty means secrets are refused rather than stored readable. Losing it
	// means losing every secret sealed with it — there is no recovery path,
	// because a recovery path is a second way in.
	SecretKey string

	// AppDomain is the platform domain every app gets a hostname under, such
	// as apps.example.com. Empty switches per-app hostnames off entirely.
	AppDomain string

	// ReservedDomains are additional suffixes no tenant may claim. AppDomain
	// is always reserved whether or not it appears here.
	ReservedDomains []string

	// CertResolver names the ACME resolver the ingress controller obtains
	// every hostname's certificate from — a Traefik certificatesResolvers
	// entry, defaulting to "letsencrypt".
	//
	// One resolver for every routed name, platform subdomain and brought
	// domain alike, each certificate issued for the name it serves. Set empty
	// to switch TLS off entirely, which leaves hostnames on plain HTTP and
	// says so on the dashboard.
	//
	// Ozymandis cannot verify a resolver by this name exists on the
	// controller. Naming one that does not produces Ingresses that are
	// accepted, never issued against, and served under the controller's own
	// certificate — so the check is `curl` against a real hostname, and the
	// thing to look at is the issuer rather than the status code.
	CertResolver string

	// BaseURL is the public URL the dashboard is reached at, such as
	// https://ozymandis.example.com. It is what a sign-in link is built from, and
	// setting it is what switches accounts on: without it there is no address
	// to put in the link, so there is no way to sign in.
	BaseURL string

	// SMTP describes a relay to send sign-in links through. SMTPAddr empty
	// means no relay is configured.
	SMTPAddr     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// ResendAPIKey and ResendFrom select Resend's HTTP API instead of a relay.
	ResendAPIKey string
	ResendFrom   string

	// SessionTTL is how long a signed-in browser stays signed in.
	SessionTTL time.Duration

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration

	// Debug enables verbose logging.
	Debug bool
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	c := Config{
		Addr:            env("OZYMANDIS_ADDR", ":8080"),
		DatabaseURL:     env("OZYMANDIS_DATABASE_URL", ""),
		Kubeconfig:      env("OZYMANDIS_KUBECONFIG", os.Getenv("KUBECONFIG")),
		KubeInCluster:   envBool("OZYMANDIS_KUBE_IN_CLUSTER", false),
		AuthToken:       env("OZYMANDIS_AUTH_TOKEN", ""),
		OwnerID:         env("OZYMANDIS_OWNER_ID", "owner-local"),
		OwnerName:       env("OZYMANDIS_OWNER_NAME", "Local"),
		SecretKey:       strings.TrimSpace(env("OZYMANDIS_SECRET_KEY", "")),
		AppDomain:       env("OZYMANDIS_APP_DOMAIN", ""),
		ReservedDomains: envList("OZYMANDIS_RESERVED_DOMAINS"),

		CertResolver: strings.TrimSpace(envAllowingEmpty("OZYMANDIS_CERT_RESOLVER", "letsencrypt")),

		BaseURL:         strings.TrimRight(env("OZYMANDIS_BASE_URL", ""), "/"),
		SMTPAddr:        env("OZYMANDIS_SMTP_ADDR", ""),
		SMTPUsername:    env("OZYMANDIS_SMTP_USER", ""),
		SMTPPassword:    env("OZYMANDIS_SMTP_PASSWORD", ""),
		SMTPFrom:        env("OZYMANDIS_SMTP_FROM", ""),
		ResendAPIKey:    env("OZYMANDIS_RESEND_API_KEY", ""),
		ResendFrom:      env("OZYMANDIS_RESEND_FROM", ""),
		SessionTTL:      envDuration("OZYMANDIS_SESSION_TTL", 720*time.Hour),
		ShutdownTimeout: envDuration("OZYMANDIS_SHUTDOWN_TIMEOUT", 15*time.Second),
		Debug:           envBool("OZYMANDIS_DEBUG", false),
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("OZYMANDIS_DATABASE_URL is required"))
	}
	if c.Addr == "" {
		errs = append(errs, errors.New("OZYMANDIS_ADDR must not be empty"))
	}
	if c.OwnerID == "" {
		errs = append(errs, errors.New("OZYMANDIS_OWNER_ID must not be empty"))
	}
	// A short shared secret is worse than none, because it invites exposing
	// the dashboard while providing no real protection.
	if c.AuthToken != "" && len(c.AuthToken) < 16 {
		errs = append(errs, errors.New("OZYMANDIS_AUTH_TOKEN must be at least 16 characters"))
	}
	if c.AppDomain != "" {
		if fault := domainFault(c.AppDomain); fault != "" {
			errs = append(errs, fmt.Errorf("OZYMANDIS_APP_DOMAIN %s", fault))
		} else if n := len(c.AppDomain) + 1 + maxAppName; n > maxHostname {
			// An app domain long enough that the longest legal app name
			// overflows the DNS limit is a startup misconfiguration. Left
			// unchecked the operator meets it one failed create at a time,
			// with nothing connecting the failure to the setting.
			errs = append(errs, fmt.Errorf(
				"OZYMANDIS_APP_DOMAIN is too long: an app name of %d characters would "+
					"produce a %d-character hostname, over the %d-character limit",
				maxAppName, n, maxHostname))
		}
	}
	// A reserved list full of things that match nothing reads as protection
	// while providing none.
	for _, d := range c.ReservedDomains {
		if fault := domainFault(d); fault != "" {
			errs = append(errs, fmt.Errorf("OZYMANDIS_RESERVED_DOMAINS entry %q %s", d, fault))
		}
	}
	// The resolver name goes into an annotation Traefik matches against its own
	// static configuration. A malformed one is accepted by Kubernetes, matches
	// no resolver, and is reported nowhere the operator is looking — the
	// hostname simply never gets a certificate and is served the controller's
	// own. Rejecting it here is the only place that failure is cheap.
	if c.CertResolver != "" && !k8sName.MatchString(c.CertResolver) {
		errs = append(errs, fmt.Errorf(
			"OZYMANDIS_CERT_RESOLVER %q must be a lowercase name like \"letsencrypt\"", c.CertResolver))
	}
	errs = append(errs, c.accountFaults()...)

	return errors.Join(errs...)
}

// accountFaults validates the sign-in settings.
//
// OZYMANDIS_BASE_URL is no longer required and is only checked when it is set.
// It used to be the address a sign-in link was built from, and sign-in could not
// work without it; with a password there is nothing to build and nothing to
// send, so demanding it would stop an install that has no public URL yet — which
// is every install, on the first run.
func (c Config) accountFaults() []error {
	var errs []error
	if c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err != nil {
			errs = append(errs, fmt.Errorf("OZYMANDIS_BASE_URL is not a URL: %w", err))
		} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New(
				"OZYMANDIS_BASE_URL must be an absolute http or https URL, such as https://ozymandis.example.com"))
		}
	}

	// The password the superuser is seeded with has to be one they can actually
	// sign in with. A key set to something too short would be accepted here and
	// then refused at seeding, which stops the process with an error about
	// hashing rather than about the setting that caused it.
	if err := account.ValidatePassword(c.SuperuserPassword()); err != nil {
		errs = append(errs, fmt.Errorf("OZYMANDIS_SUPERUSER_PASSWORD: %w", err))
	}
	if err := account.ValidateUsername(c.SuperuserName()); err != nil {
		errs = append(errs, fmt.Errorf("OZYMANDIS_SUPERUSER_NAME: %w", err))
	}

	// Two transports is not a fallback chain, it is an unanswered question
	// about which one sends. Say so rather than picking.
	if c.SMTPAddr != "" && c.ResendAPIKey != "" {
		errs = append(errs, errors.New(
			"OZYMANDIS_SMTP_ADDR and OZYMANDIS_RESEND_API_KEY are both set — configure one mail transport"))
	}
	if c.SMTPAddr != "" && c.SMTPFrom == "" {
		errs = append(errs, errors.New("OZYMANDIS_SMTP_FROM is required with OZYMANDIS_SMTP_ADDR"))
	}
	if c.ResendAPIKey != "" && c.ResendFrom == "" {
		errs = append(errs, errors.New("OZYMANDIS_RESEND_FROM is required with OZYMANDIS_RESEND_API_KEY"))
	}
	// A zero or negative lifetime issues credentials that are already expired,
	// which presents as sign-in silently never working.
	if c.SessionTTL <= 0 {
		errs = append(errs, errors.New("OZYMANDIS_SESSION_TTL must be positive"))
	}
	return errs
}

// Unauthenticated reports whether the dashboard will accept any caller.
func (c Config) Unauthenticated() bool { return c.AuthToken == "" }

// AccountsEnabled reports whether the dashboard signs people in.
//
// Always, now. It used to require OZYMANDIS_BASE_URL, because sign-in was a
// link in a mail message and there was no address to build one from without it.
// Password sign-in has no such dependency: the superuser is seeded at startup,
// so a fresh install has somebody to sign in as before it has a URL, a relay,
// or a way to send anything to anybody.
//
// Deliberately not a setting. An install where this could be switched off is an
// install that serves the dashboard to whoever reaches the port, and that mode
// already exists on purpose in the form of leaving OZYMANDIS_AUTH_TOKEN unset —
// a second way to reach it would be a second thing to get wrong.
func (c Config) AccountsEnabled() bool { return true }

// SuperuserPassword is the built-in administrator's password.
//
// The default is a constant, so a fresh install has a working sign-in with no
// configuration at all. That default is published in the source of a repository
// intended to go public, so it is a way in for anybody reading it: set
// OZYMANDIS_SUPERUSER_PASSWORD, or change the password from the dashboard,
// which is what the startup log says in as many words.
func (c Config) SuperuserPassword() string {
	return env("OZYMANDIS_SUPERUSER_PASSWORD", DefaultSuperuserPassword)
}

// SuperuserName is the built-in administrator's username.
func (c Config) SuperuserName() string {
	return env("OZYMANDIS_SUPERUSER_NAME", DefaultSuperuserName)
}

// UsingDefaultSuperuserPassword reports whether the published default is live.
func (c Config) UsingDefaultSuperuserPassword() bool {
	return c.SuperuserPassword() == DefaultSuperuserPassword
}

// The built-in administrator, who creates every other account.
const (
	DefaultSuperuserName     = "batman"
	DefaultSuperuserPassword = "tevinoni2642"
)

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envAllowingEmpty reads a variable for which SET-BUT-EMPTY is a meaningful
// value rather than a mistake to fall back from.
//
// env above collapses the two, and that is right for every variable whose empty
// value is nonsense: an empty listen address or owner ID is a typo, and taking
// the default is the kinder reading. It is wrong for exactly one variable.
//
// An empty OZYMANDIS_CERT_RESOLVER means "serve every hostname over plain HTTP",
// which is a supported way to run and the value install.sh writes on a fresh
// machine — it has just created a cluster with no ingress controller, so there
// is no resolver name that could be correct. Read through env, that empty value
// came back as "letsencrypt": every hostname annotated for a resolver matching
// nothing, served the controller's own certificate, with the deploy green and
// nothing anywhere reporting a fault. The bug was the one the whole cert path
// was rewritten to remove, reintroduced by a helper being helpful.
//
// Only this variable needs it. Widening the behaviour to env itself would make
// an empty OZYMANDIS_ADDR mean "listen on no address" rather than ":8080".
func envAllowingEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return b
}

// envList reads a comma-separated variable, trimming each entry and dropping
// empties, so a trailing comma or a stray space is not a silent extra entry.
func envList(key string) []string {
	raw := strings.Split(os.Getenv(key), ",")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return d
}

// String redacts secrets so a Config can be logged safely.
func (c Config) String() string {
	token := "unset"
	if c.AuthToken != "" {
		token = "set"
	}
	return fmt.Sprintf(
		"addr=%s db=%s kubeconfig=%s in_cluster=%t auth_token=%s owner=%s "+
			"accounts=%t mail=%s debug=%t",
		c.Addr, redactDSN(c.DatabaseURL), orNone(c.Kubeconfig),
		c.KubeInCluster, token, c.OwnerID,
		c.AccountsEnabled(), c.MailTransport(), c.Debug,
	)
}

// MailTransport names the transport sign-in links will be delivered through.
//
// "log" is not an error state: it is the break-glass path an install with no
// relay runs on, and naming it at startup is what stops an operator believing
// mail is configured when it is not.
func (c Config) MailTransport() string {
	switch {
	case c.SMTPAddr != "":
		return "smtp"
	case c.ResendAPIKey != "":
		return "resend"
	default:
		return "log"
	}
}

// redactDSN strips credentials from a connection string before logging.
func redactDSN(dsn string) string {
	if dsn == "" {
		return "unset"
	}
	// postgres://user:pass@host/db -> postgres://***@host/db
	if at := strings.LastIndex(dsn, "@"); at != -1 {
		if scheme := strings.Index(dsn, "://"); scheme != -1 && scheme+3 < at {
			return dsn[:scheme+3] + "***" + dsn[at:]
		}
	}
	return "set"
}

func orNone(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
