// Package appspec is the ozymandis.toml file: the part of an app's
// configuration that lives with its code.
//
// # Absent versus zero
//
// EVERY OPTIONAL FIELD IN THIS PACKAGE IS A POINTER, A SLICE, OR A MAP. There
// are no optional scalar fields, and adding one is a bug that TestEveryOptional
// FieldCanBeAbsent will fail the build over.
//
// The rule exists because this file is a partial description by design — it
// says what it wants to say and leaves the rest alone — so "the file did not
// mention replicas" and "the file set replicas to 0" are opposite instructions
// and must never collapse into the same value. They do collapse in the obvious
// encoding: decoding into a struct of plain values gives Replicas == 0 for
// `replicas = 0`, for a `[scale]` table with no key, and for no `[scale]` table
// at all. Three different intentions, one indistinguishable result, and the
// deploy that scales production to nothing looks exactly like the deploy that
// said nothing about scaling.
//
// BurntSushi/toml offers a second mechanism for this — toml.MetaData.IsDefined
// — which keeps the struct free of pointers. It is deliberately not used, and
// the reason is the API boundary rather than taste: MetaData describes one
// decode, and it is gone the moment the CLI marshals a Spec to JSON for
// PUT /api/v1/apps/{name}/config. The server would have to reconstruct presence
// it was never sent, or the CLI would have to ship a parallel list of set field
// paths beside the values — a second representation of the same fact, and the
// two would drift. A pointer survives the wire as null-or-present with no help
// from anyone.
//
// What matters most is that there is ONE rule. A file where half the fields are
// pointers and half rely on zero-value checks is one where no reviewer can tell
// which discipline any given field follows, and that ambiguity is the actual
// hazard — worse than either mechanism chosen consistently.
//
// # What "authoritative" covers
//
// The file is authoritative over what it can COMPLETELY describe, and a config
// converge applies a subset of that. The scope is written here rather than left
// in a design conversation, because somebody reading this type needs to know
// which of their edits will actually take effect:
//
// CONVERGED BY A CONFIG PUT, on every deploy — drift made in the dashboard is
// reverted:
//   - [service] port, internal
//   - [health]  path, liveness
//   - [env]     the complete set of plaintext variables, INCLUDING removals
//
// RECORDED AND REPORTED, BUT APPLIED BY A DEPLOY:
//   - [build] repo, branch, subdir, image
//
// These are not unsupported. Changing an app's source or image needs an image
// pulled or built and a deployment row recording which commit produced what is
// running — that is the deploy path, not a settings reconcile. A converge
// reports the difference with that reason and leaves it; `oz deploy` applies it.
//
// NEVER CONVERGED — the operational axis:
//   - [scale] replicas, applied at creation and thereafter only on request
//   - [[volumes]] size, which only ever grows
//
// NOT OWNED BY THE FILE AT ALL:
//   - secrets, which are not representable here at any size
//   - [[domains]], which are ADDITIVE: a host the file names is added, a host
//     the app has that the file lacks is left alone and reported
//
// # What this package deliberately does not do
//
// It does not validate repository URLs, command lines, or hostnames. Those
// rules live in the app package, which is the authority, and the server applies
// them on every request whatever a client did first. Restating them here would
// create a second set of rules to keep in agreement, and the server cannot
// trust a client's validation anyway — so the only thing duplication would buy
// is the chance for the two to disagree.
//
// This package therefore imports nothing heavier than the TOML decoder and the
// orchestrator's own label rule, which keeps the CLI binary free of the
// database and Kubernetes client libraries the server needs.
package appspec

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FileName is what the CLI looks for in a repository.
const FileName = "ozymandis.toml"

// Spec is a parsed ozymandis.toml.
//
// It is also the JSON body of PUT /api/v1/apps/{name}/config, which is why the
// json tags carry omitempty everywhere: an absent section must marshal to an
// absent key, not to a null the server would have to treat as a third state.
type Spec struct {
	Name string `toml:"name" json:"name"`

	Build   *Build   `toml:"build" json:"build,omitempty"`
	Deploy  *Deploy  `toml:"deploy" json:"deploy,omitempty"`
	Service *Service `toml:"service" json:"service,omitempty"`
	Health  *Health  `toml:"health" json:"health,omitempty"`
	Scale   *Scale   `toml:"scale" json:"scale,omitempty"`

	// Env is the complete set of PLAINTEXT variables when present.
	//
	// A map, so nil (no [env] table) and empty (an [env] table with nothing in
	// it, meaning "remove them all") stay distinguishable — the same rule the
	// pointers follow. Secrets are not representable here at any size; see
	// ErrSecretsInFile.
	Env map[string]string `toml:"env" json:"env,omitempty"`

	Volumes []Volume `toml:"volumes" json:"volumes,omitempty"`
	Domains []Domain `toml:"domains" json:"domains,omitempty"`
}

// Build is where the image comes from.
type Build struct {
	// Repo and Image are mutually exclusive: an app is built from source or
	// pulled, and a file naming both has not said which.
	Repo   *string `toml:"repo" json:"repo,omitempty"`
	Branch *string `toml:"branch" json:"branch,omitempty"`
	Subdir *string `toml:"subdir" json:"subdir,omitempty"`
	Image  *string `toml:"image" json:"image,omitempty"`
}

// Deploy is what happens on the way out.
type Deploy struct {
	// ReleaseCommand runs against the new image before traffic moves to it.
	// Empty string is meaningful and different from absent: it clears a
	// release command that was previously set.
	ReleaseCommand *string `toml:"release_command" json:"release_command,omitempty"`
}

// Service is how the app is reached.
type Service struct {
	Port     *int32 `toml:"port" json:"port,omitempty"`
	Internal *bool  `toml:"internal" json:"internal,omitempty"`
}

// Health is the readiness probe.
type Health struct {
	Path     *string `toml:"path" json:"path,omitempty"`
	Liveness *bool   `toml:"liveness" json:"liveness,omitempty"`
}

// Scale is the OPERATIONAL axis, and the file does not own it.
//
// Present in the file so an app created from one starts at the right size, and
// deliberately not converged afterwards. Replicas is the single field a person
// changes while something is on fire, and the correct value is a property of
// right now rather than of the commit — so a deploy from a checkout that says 2
// must not undo an emergency scale to 10. `oz deploy --scale` applies it
// explicitly; nothing else does.
type Scale struct {
	Replicas *int32 `toml:"replicas" json:"replicas,omitempty"`
}

// Volume is storage. Sizes grow and never shrink, so this axis is operational
// too: the file's size is a floor applied at creation, not a target converged to.
type Volume struct {
	Name string  `toml:"name" json:"name"`
	Path string  `toml:"path" json:"path"`
	Size *string `toml:"size" json:"size,omitempty"`
}

// Domain is a hostname. Additive: see Spec.Validate and the convergence rules
// on the server. A host in the file that the app lacks is added; a host on the
// app that the file lacks is left alone and reported, because deleting a line
// from a config file should not silently drop a certificate and start returning
// NXDOMAIN to live traffic.
type Domain struct {
	Host string `toml:"host" json:"host"`
}

// ErrSecretsInFile is returned for a file that tries to carry credentials.
//
// A named error rather than a generic parse failure because the message has to
// teach: somebody writing [secrets] into a file they are about to commit is one
// keystroke from putting a production password in a repository, and "unknown
// key" would not stop them doing it in a differently-named table.
var ErrSecretsInFile = errors.New(
	"appspec: " + FileName + " must not contain secrets.\n" +
		"This file is committed to your repository; anything in it is readable by " +
		"everyone with access to the code, forever, including in the history after " +
		"you delete it.\n" +
		"Set them with `oz secrets set KEY=value` instead — they are sealed at rest " +
		"and have no read path.")

// Load reads and parses a spec from disk.
func Load(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("appspec: read %s: %w", path, err)
	}
	return Parse(data)
}

// sizeRE matches a Kubernetes-style quantity: 10Gi, 512Mi, 1T.
var sizeRE = regexp.MustCompile(`^([0-9]+)([KMGTP]i?)?$`)

var sizeUnits = map[string]int64{
	"":   1,
	"K":  1000,
	"Ki": 1 << 10,
	"M":  1000 * 1000,
	"Mi": 1 << 20,
	"G":  1000 * 1000 * 1000,
	"Gi": 1 << 30,
	"T":  1000 * 1000 * 1000 * 1000,
	"Ti": 1 << 40,
	"P":  1000 * 1000 * 1000 * 1000 * 1000,
	"Pi": 1 << 50,
}

// ParseSize turns a quantity into bytes.
//
// Exported because the server needs the same answer the CLI showed. A size the
// two disagree about is a volume that reads as one number in a diff and is
// created as another.
func ParseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("appspec: %q is not a size — try 10Gi, 512Mi or 1T", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("appspec: %q is too large a size", s)
	}
	unit, ok := sizeUnits[m[2]]
	if !ok {
		return 0, fmt.Errorf("appspec: %q is not a unit this understands", m[2])
	}
	if n != 0 && n > (1<<62)/unit {
		return 0, fmt.Errorf("appspec: %q is too large a size", s)
	}
	return n * unit, nil
}

// Validate checks the shape of a spec.
//
// Shape only — required fields, mutually exclusive fields, and values this
// package can check without knowing anything about a cluster. The semantic
// rules (is that a clonable repository, is that a parseable command line) are
// the app package's and are applied by the server. See the package doc.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("appspec: name is required")
	}

	if b := s.Build; b != nil {
		if b.Repo != nil && b.Image != nil {
			return errors.New("appspec: [build] names both repo and image — " +
				"an app is either built from source or pulled, not both")
		}
		if b.Repo == nil && (b.Branch != nil || b.Subdir != nil) {
			return errors.New("appspec: [build] sets branch or subdir with no repo")
		}
	}

	if sv := s.Service; sv != nil && sv.Port != nil {
		if *sv.Port < 1 || *sv.Port > 65535 {
			return fmt.Errorf("appspec: port %d is not a port", *sv.Port)
		}
	}

	if h := s.Health; h != nil && h.Path != nil && *h.Path != "" {
		if !strings.HasPrefix(*h.Path, "/") {
			return fmt.Errorf("appspec: health path %q must start with /", *h.Path)
		}
	}

	if sc := s.Scale; sc != nil && sc.Replicas != nil && *sc.Replicas < 0 {
		return fmt.Errorf("appspec: replicas %d must not be negative", *sc.Replicas)
	}

	seenVol := map[string]bool{}
	seenPath := map[string]bool{}
	for _, v := range s.Volumes {
		if v.Name == "" || v.Path == "" {
			return errors.New("appspec: every [[volumes]] entry needs a name and a path")
		}
		if seenVol[v.Name] {
			return fmt.Errorf("appspec: two volumes named %q", v.Name)
		}
		if seenPath[v.Path] {
			return fmt.Errorf("appspec: two volumes mounted at %q", v.Path)
		}
		if !strings.HasPrefix(v.Path, "/") || v.Path == "/" {
			return fmt.Errorf("appspec: volume path %q must be absolute and not /", v.Path)
		}
		if v.Size != nil {
			if _, err := ParseSize(*v.Size); err != nil {
				return err
			}
		}
		seenVol[v.Name], seenPath[v.Path] = true, true
	}

	seenHost := map[string]bool{}
	for _, d := range s.Domains {
		if strings.TrimSpace(d.Host) == "" {
			return errors.New("appspec: every [[domains]] entry needs a host")
		}
		if seenHost[d.Host] {
			return fmt.Errorf("appspec: %q is listed twice", d.Host)
		}
		seenHost[d.Host] = true
	}

	for k := range s.Env {
		if strings.TrimSpace(k) == "" {
			return errors.New("appspec: [env] has an entry with no name")
		}
	}

	return nil
}

// EnvKeys returns the plaintext variable names, sorted.
//
// Sorted so that a diff built from a spec is stable between runs: map order is
// randomised in Go, and a dry-run that listed the same changes in a different
// order each time would be unreadable as a diff.
func (s Spec) EnvKeys() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
