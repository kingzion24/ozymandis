package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// A database URL is required by validate(); supply one so these tests are
	// about the new variables and nothing else.
	t.Setenv("OZYMANDIS_DATABASE_URL", "postgres://localhost/ozymandis?sslmode=disable")
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestAppDomainDefaultsToOff(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "" {
		t.Fatalf("AppDomain = %q, want empty by default", c.AppDomain)
	}
	if c.CertResolver != "letsencrypt" {
		t.Fatalf("CertResolver = %q, want letsencrypt by default", c.CertResolver)
	}
}

func TestAppDomainAndReservedDomainsLoad(t *testing.T) {
	setEnv(t, map[string]string{
		"OZYMANDIS_APP_DOMAIN":       "apps.example.com",
		"OZYMANDIS_RESERVED_DOMAINS": "internal.example.com, admin.example.com",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "apps.example.com" {
		t.Fatalf("AppDomain = %q", c.AppDomain)
	}
	if len(c.ReservedDomains) != 2 ||
		c.ReservedDomains[0] != "internal.example.com" ||
		c.ReservedDomains[1] != "admin.example.com" {
		t.Fatalf("ReservedDomains = %v, want both entries trimmed", c.ReservedDomains)
	}
}

// A resolver name that cannot match anything is refused at startup.
//
// This is the only place the mistake is cheap. A malformed name is accepted by
// Kubernetes, written into the annotation, matched by no resolver, and reported
// nowhere — the hostname is served the controller's own certificate and every
// deploy stays green. Catching it here turns a silent TLS failure into a
// process that will not start.
func TestMalformedCertResolverFails(t *testing.T) {
	for _, bad := range []string{"Lets Encrypt", "LETSENCRYPT", "le_prod", "letsencrypt!"} {
		setEnv(t, map[string]string{"OZYMANDIS_CERT_RESOLVER": bad})
		_, err := Load()
		if err == nil {
			t.Fatalf("Load accepted OZYMANDIS_CERT_RESOLVER=%q", bad)
		}
		if !strings.Contains(err.Error(), "OZYMANDIS_CERT_RESOLVER") {
			t.Fatalf("error should name the variable, got: %v", err)
		}
	}
}

func TestCertResolverAcceptsAValidNameAndEmpty(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_CERT_RESOLVER": "corporate-ca"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CertResolver != "corporate-ca" {
		t.Fatalf("CertResolver = %q, want corporate-ca", c.CertResolver)
	}

	// Empty is supported, not a mistake: it serves every hostname over plain
	// HTTP, which is the honest state for an install whose controller has no
	// resolver yet. Rejecting it would leave no way to run without TLS.
	setEnv(t, map[string]string{"OZYMANDIS_CERT_RESOLVER": ""})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load rejected an empty resolver: %v", err)
	}
	if c.CertResolver != "" {
		t.Fatalf("CertResolver = %q, want empty", c.CertResolver)
	}
}

func TestMalformedAppDomainFails(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_APP_DOMAIN": "not a domain"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed OZYMANDIS_APP_DOMAIN")
	}
}

// Every variable config.go reads must be documented, or an operator finds out
// a setting exists by reading the source.
func TestEveryVariableIsDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	for _, name := range []string{
		"OZYMANDIS_APP_DOMAIN", "OZYMANDIS_CERT_RESOLVER", "OZYMANDIS_RESERVED_DOMAINS",
	} {
		if !strings.Contains(string(src), name) {
			t.Fatalf("%s is not read by config.go", name)
		}
		if !strings.Contains(string(example), name) {
			t.Fatalf("%s is not documented in .env.example", name)
		}
	}
}

// An app domain that cannot produce a valid hostname is a startup
// misconfiguration, not a per-app runtime failure. Without this check the
// operator learns about it only when a create fails, one app at a time.
func TestOverlongAppDomainFailsAtStartup(t *testing.T) {
	cases := map[string]string{
		"whole name too long for any app": strings.Repeat("a", 120) + "." +
			strings.Repeat("b", 120) + ".example.com",
		"single label over 63": strings.Repeat("a", 70) + ".example.com",
	}

	for name, domain := range cases {
		t.Run(name, func(t *testing.T) {
			setEnv(t, map[string]string{"OZYMANDIS_APP_DOMAIN": domain})
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted an app domain that can never yield a hostname (len=%d)", len(domain))
			}
		})
	}
}

func TestUsableAppDomainStillLoads(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_APP_DOMAIN": "apps.example.com"})
	if _, err := Load(); err != nil {
		t.Fatalf("Load rejected a perfectly ordinary app domain: %v", err)
	}
}

// Garbage in a reserved list is worse than an empty one: it reads as though
// names are protected while matching nothing.
func TestReservedDomainsAreValidated(t *testing.T) {
	for _, bad := range []string{"not a domain", "https://evil.test", "*", "evil.test/path"} {
		t.Run(bad, func(t *testing.T) {
			setEnv(t, map[string]string{"OZYMANDIS_RESERVED_DOMAINS": bad})
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %q as a reserved domain", bad)
			}
		})
	}
}

func TestValidReservedDomainsLoad(t *testing.T) {
	setEnv(t, map[string]string{
		"OZYMANDIS_RESERVED_DOMAINS": "internal.example.com, admin.example.com",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.ReservedDomains) != 2 {
		t.Fatalf("ReservedDomains = %v, want 2", c.ReservedDomains)
	}
}

// Sign-in no longer depends on anything being deliverable. It used to be a
// link, so it needed a URL to put in it and a relay to send it through, and an
// install with neither had to keep accounts off or lock its operator out. A
// password needs none of that, and the superuser is seeded at startup — so
// accounts are on always, including on an install that has no public URL yet.
func TestAccountsAreOnWithoutABaseURL(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AccountsEnabled() {
		t.Fatal("accounts are off with no base URL — nobody can sign in at all")
	}
	if c.SuperuserName() != DefaultSuperuserName {
		t.Fatalf("SuperuserName = %q, want the built-in default", c.SuperuserName())
	}
	if !c.UsingDefaultSuperuserPassword() {
		t.Fatal("the default password is not reported as in use, so nothing warns about it")
	}
}

// The seeded password has to be one the person can actually sign in with. A
// value refused at seeding stops the process with an error about hashing rather
// than about the setting that caused it.
func TestSuperuserPasswordIsValidated(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_SUPERUSER_PASSWORD": "short"})
	if _, err := Load(); err == nil {
		t.Fatal("a password too short to be accepted was loaded without complaint")
	}
}

func TestSuperuserPasswordCanBeOverridden(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_SUPERUSER_PASSWORD": "a-chosen-password"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UsingDefaultSuperuserPassword() {
		t.Fatal("an overridden password still reports as the built-in default")
	}
}

func TestSMTPAndResendTogetherFail(t *testing.T) {
	setEnv(t, map[string]string{
		"OZYMANDIS_BASE_URL":       "https://ozymandis.example.test",
		"OZYMANDIS_SMTP_ADDR":      "smtp.example.test:587",
		"OZYMANDIS_SMTP_FROM":      "ozymandis@example.test",
		"OZYMANDIS_RESEND_API_KEY": "re_123",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted two mail transports at once")
	}
}

func TestSMTPWithoutFromFails(t *testing.T) {
	setEnv(t, map[string]string{
		"OZYMANDIS_BASE_URL":  "https://ozymandis.example.test",
		"OZYMANDIS_SMTP_ADDR": "smtp.example.test:587",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted SMTP with no from address")
	}
}

func TestMalformedBaseURLFails(t *testing.T) {
	setEnv(t, map[string]string{"OZYMANDIS_BASE_URL": "not a url"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed base URL")
	}
}

// TestEveryVariableReadIsDocumented scans for every OZYMANDIS_* string config.go
// reads, rather than checking a list someone has to remember to extend. The
// hardcoded version passed while OZYMANDIS_SHUTDOWN_TIMEOUT went undocumented.
func TestEveryVariableReadIsDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	found := regexp.MustCompile(`"(OZYMANDIS_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no OZYMANDIS_* variables found in config.go — the scan is broken")
	}

	seen := map[string]bool{}
	for _, m := range found {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(string(example), name) {
			t.Errorf("%s is read by config.go but not documented in .env.example", name)
		}
	}
}
