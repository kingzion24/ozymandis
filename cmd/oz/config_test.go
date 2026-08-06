package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withConfigHome points the config at a temporary directory.
//
// XDG_CONFIG_HOME rather than HOME, because os.UserConfigDir consults it first
// on Linux — and a test that wrote to a real HOME would clobber the developer's
// own credentials, which is the kind of test failure people remember.
func withConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("OZ_CONTEXT", "")
	return dir
}

func TestConfigRoundTrip(t *testing.T) {
	withConfigHome(t)

	cfg := Config{
		Current: "prod",
		Contexts: map[string]Context{
			"prod":    {Endpoint: "https://prod.example", Token: "oz_prod"},
			"staging": {Endpoint: "https://staging.example", Token: "oz_staging"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if back.Current != "prod" || len(back.Contexts) != 2 {
		t.Fatalf("config = %+v", back)
	}
	if back.Contexts["prod"].Token != "oz_prod" {
		t.Errorf("token did not survive: %+v", back.Contexts["prod"])
	}
}

// The file holds credentials for every install a person can reach.
func TestConfigIsWrittenPrivate(t *testing.T) {
	withConfigHome(t)

	cfg := Config{Current: "a", Contexts: map[string]Context{
		"a": {Endpoint: "https://a", Token: "oz_secret"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, _ := ConfigPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600 — it holds every token this "+
			"machine can authenticate with", perm)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("config directory mode = %o, want 700", perm)
	}
}

// A missing file is the state before the first login, not a failure.
func TestNoConfigIsNotAnError(t *testing.T) {
	withConfigHome(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with no file: %v", err)
	}
	if len(cfg.Contexts) != 0 {
		t.Errorf("contexts = %v", cfg.Contexts)
	}

	// And resolving says how to fix it, naming the command.
	_, _, err = cfg.Resolve("")
	if !errors.Is(err, errNotLoggedIn) {
		t.Fatalf("err = %v, want errNotLoggedIn", err)
	}
	if !strings.Contains(err.Error(), "oz auth login") {
		t.Errorf("the message does not name the command that fixes it: %v", err)
	}
}

// Precedence: flag beats environment beats saved current.
//
// The more deliberate the act, the more it wins — so a one-off `--context
// staging` cannot be overridden by something set in a shell profile months ago.
// Getting this backwards means deploying to the wrong install, which is the
// mistake the whole context mechanism exists to prevent.
func TestContextPrecedence(t *testing.T) {
	withConfigHome(t)

	cfg := Config{
		Current: "saved",
		Contexts: map[string]Context{
			"saved": {Endpoint: "https://saved", Token: "t"},
			"env":   {Endpoint: "https://env", Token: "t"},
			"flag":  {Endpoint: "https://flag", Token: "t"},
		},
	}

	t.Run("saved current when nothing else", func(t *testing.T) {
		name, _, err := cfg.Resolve("")
		if err != nil || name != "saved" {
			t.Fatalf("name = %q, err = %v", name, err)
		}
	})

	t.Run("environment beats saved", func(t *testing.T) {
		t.Setenv("OZ_CONTEXT", "env")
		name, _, err := cfg.Resolve("")
		if err != nil || name != "env" {
			t.Fatalf("name = %q, err = %v", name, err)
		}
	})

	t.Run("flag beats environment", func(t *testing.T) {
		t.Setenv("OZ_CONTEXT", "env")
		name, _, err := cfg.Resolve("flag")
		if err != nil || name != "flag" {
			t.Fatalf("name = %q, err = %v", name, err)
		}
	})
}

func TestUnknownContextListsTheRealOnes(t *testing.T) {
	cfg := Config{Contexts: map[string]Context{
		"prod": {Endpoint: "https://a", Token: "t"},
		"dev":  {Endpoint: "https://b", Token: "t"},
	}}

	_, _, err := cfg.Resolve("typo")
	if err == nil {
		t.Fatal("an unknown context resolved")
	}
	for _, want := range []string{"typo", "prod", "dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
}

// A half-written context is worse than none: it fails at the request with a
// 401 rather than at resolution with an explanation.
func TestIncompleteContextIsRefused(t *testing.T) {
	cfg := Config{Current: "half", Contexts: map[string]Context{
		"half": {Endpoint: "https://a"}, // no token
	}}
	if _, _, err := cfg.Resolve(""); err == nil {
		t.Error("a context with no token resolved")
	}
}

// --context is accepted before the subcommand as well as after.
func TestExtractContext(t *testing.T) {
	cases := []struct {
		args     []string
		wantArgs []string
		wantName string
	}{
		{[]string{"--context", "prod", "deploy"}, []string{"deploy"}, "prod"},
		{[]string{"deploy", "--context", "prod"}, []string{"deploy"}, "prod"},
		{[]string{"--context=prod", "deploy"}, []string{"deploy"}, "prod"},
		{[]string{"deploy", "--watch"}, []string{"deploy", "--watch"}, ""},
		{[]string{"deploy"}, []string{"deploy"}, ""},
	}

	for _, tc := range cases {
		args, name := extractContext(tc.args)
		if name != tc.wantName {
			t.Errorf("%v: name = %q, want %q", tc.args, name, tc.wantName)
		}
		if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") {
			t.Errorf("%v: args = %v, want %v", tc.args, args, tc.wantArgs)
		}
	}
}

// Every registered command has help text, so `oz` with no arguments is a
// usable index rather than a list of bare names.
func TestEveryCommandIsDocumented(t *testing.T) {
	if len(commands) == 0 {
		t.Fatal("no commands registered")
	}
	for name, c := range commands {
		if c.usage == "" {
			t.Errorf("%s has no usage line", name)
		}
		if c.summary == "" {
			t.Errorf("%s has no summary", name)
		}
		if c.run == nil {
			t.Errorf("%s does nothing", name)
		}
		if !strings.HasPrefix(c.usage, name) {
			t.Errorf("%s: usage %q does not start with the command name", name, c.usage)
		}
	}
}
