package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is what `oz auth login` writes.
//
// Contexts rather than a single endpoint, because the people most likely to use
// a CLI against a self-hosted PaaS are the ones running more than one of them —
// a staging box and a production box, at least. A tool that holds one endpoint
// makes the second install an exercise in editing a dotfile between commands,
// and the mistake that produces is deploying to the wrong one.
type Config struct {
	// Current names the context used when none is given.
	Current string `toml:"current"`

	Contexts map[string]Context `toml:"contexts"`
}

// Context is one install.
type Context struct {
	Endpoint string `toml:"endpoint"`

	// Token is an API token. Stored in a 0600 file under the user's config
	// directory — the same posture ssh and kube keep, and the reason the file
	// is written with an explicit mode rather than whatever the umask says.
	Token string `toml:"token"`
}

// ConfigPath is where the config lives.
//
// Under XDG_CONFIG_HOME when set, which is what os.UserConfigDir already
// implements, so a person who has moved their dotfiles finds this where they
// expect the rest of them.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("oz: cannot find your config directory: %w", err)
	}
	return filepath.Join(dir, "ozymandis", "config.toml"), nil
}

// LoadConfig reads the config, returning an empty one if there is none.
//
// A missing file is not an error: it is the state before `oz auth login`, and
// every command that needs credentials says so with a message naming that
// command rather than with a stat error.
func LoadConfig() (Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{Contexts: map[string]Context{}}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("oz: read %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("oz: %s is not valid TOML: %w", path, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	return cfg, nil
}

// Save writes the config with a mode that keeps the token to its owner.
//
// Written to a temporary file and renamed, so an interrupted write leaves the
// previous config rather than a truncated one. Losing a token to a half-written
// file means re-running login; losing it to a file that parses as valid TOML
// with a truncated token means a confusing 401 instead.
func (c Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("oz: create config directory: %w", err)
	}

	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return fmt.Errorf("oz: encode config: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("oz: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("oz: replace %s: %w", path, err)
	}
	return nil
}

// Resolve returns the context to act as.
//
// Precedence is flag, then OZ_CONTEXT, then the saved current. A flag beats an
// environment variable beats a file: the more deliberate the act, the more it
// wins, so a one-off `--context staging` cannot be silently overridden by
// something set in a shell profile months ago.
func (c Config) Resolve(name string) (string, Context, error) {
	if name == "" {
		name = os.Getenv("OZ_CONTEXT")
	}
	if name == "" {
		name = c.Current
	}
	if name == "" {
		return "", Context{}, errNotLoggedIn
	}

	ctx, ok := c.Contexts[name]
	if !ok {
		return "", Context{}, fmt.Errorf(
			"oz: no context called %q. You have: %s", name, strings.Join(c.Names(), ", "))
	}
	if ctx.Endpoint == "" || ctx.Token == "" {
		return "", Context{}, fmt.Errorf(
			"oz: context %q is missing an endpoint or a token — run `oz auth login` again",
			name)
	}
	return name, ctx, nil
}

// Names lists the configured contexts, sorted.
func (c Config) Names() []string {
	names := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// errNotLoggedIn is the state before the first login.
//
// Its own error so that every command can report it the same way, naming the
// command that fixes it. "unauthorized" from a CLI that has never been given a
// credential is a true statement that helps nobody.
var errNotLoggedIn = fmt.Errorf(
	"oz: not logged in. Run:\n\n" +
		"    oz auth login --endpoint https://your-install --token oz_…\n\n" +
		"Create a token under Settings › Access tokens in the dashboard.")
