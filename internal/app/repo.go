package app

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DefaultBranch is what a repository is built from when nobody says.
//
// A default rather than a required field: almost every repository uses it, and
// a form that refuses to submit without a value everyone would type the same
// way is a form asking a question it already knows the answer to.
const DefaultBranch = "main"

// Repo is where an app's source comes from.
//
// A value type rather than three columns threaded through every signature,
// because the three only ever travel together and only ever mean anything
// together: a subdirectory with no URL is not a partial configuration, it is a
// mistake.
type Repo struct {
	// URL is an https:// or ssh:// Git URL.
	URL string

	// Branch is the ref built. Empty means DefaultBranch.
	Branch string

	// Subdir is the directory inside the repository to build from, for a
	// monorepo. Empty is the root.
	Subdir string
}

// Set reports whether this app builds from source.
func (r Repo) Set() bool { return r.URL != "" }

// Ref is the branch actually used.
func (r Repo) Ref() string {
	if r.Branch == "" {
		return DefaultBranch
	}
	return r.Branch
}

// A branch, tag, or anything else `git clone --branch` accepts. Refusing the
// rest is not about Git's own rules: this value is passed to a clone inside a
// build container, so the set is narrowed to what cannot be read as anything
// but a ref.
var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// A monorepo path. No leading slash, no traversal, no backslashes — the value
// is joined onto a checkout directory inside the builder, and ".." there walks
// out of the repository into the build container's filesystem.
var subdirRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// Validate checks a repository somebody typed.
func (r Repo) Validate() error {
	if !r.Set() {
		return nil
	}

	u, err := url.Parse(r.URL)
	if err != nil {
		return fmt.Errorf("app: %q is not a URL", r.URL)
	}
	switch u.Scheme {
	case "https", "ssh":
	case "http":
		// Refused rather than upgraded. A repository fetched over plain HTTP
		// can be replaced in transit by anyone on the path, and what comes back
		// is built and run in this cluster.
		return errors.New("app: use https rather than http — " +
			"a repository fetched in the clear can be replaced on the way here, " +
			"and what arrives gets built and run")
	case "":
		return errors.New("app: a repository URL needs a scheme, e.g. " +
			"https://github.com/you/app.git")
	default:
		return fmt.Errorf("app: %q is not a scheme this can clone — https or ssh", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("app: %q has no host", r.URL)
	}

	if r.Branch != "" && !refRE.MatchString(r.Branch) {
		return fmt.Errorf("app: %q is not a branch name", r.Branch)
	}
	if r.Subdir != "" {
		if !subdirRE.MatchString(r.Subdir) || strings.Contains(r.Subdir, "..") {
			return fmt.Errorf("app: %q is not a path inside the repository", r.Subdir)
		}
	}
	return nil
}

// Normalise trims a repository the way it will be stored.
func (r Repo) Normalise() Repo {
	return Repo{
		URL:    strings.TrimSpace(r.URL),
		Branch: strings.TrimSpace(r.Branch),
		Subdir: strings.Trim(strings.TrimSpace(r.Subdir), "/"),
	}
}

// RepoIdentity is which repository an app came from, split into the parts a
// person recognises.
//
// Parsed rather than stored: the URL is already on every app that builds from
// source, and a second column recording the same fact is a second thing that
// can disagree with the first.
type RepoIdentity struct {
	// Host is the forge, without a port: "github.com".
	Host string

	// Owner is everything between the host and the last path segment. Usually
	// one account, but a GitLab subgroup nests, and the whole path is what
	// makes two repositories with the same last segment different.
	Owner string

	// Name is the last path segment, without ".git": "mali-daftari-dashboard".
	Name string
}

// Set reports whether a URL parsed into anything nameable.
func (i RepoIdentity) Set() bool { return i.Name != "" }

// Path is what a forge calls the repository: "harehaDET/mali-daftari-dashboard".
func (i RepoIdentity) Path() string {
	if i.Owner == "" {
		return i.Name
	}
	return i.Owner + "/" + i.Name
}

// String is the whole identity, host included, for showing next to a project.
//
// The host is part of it because two forges can carry the same owner and name,
// and a project claiming to be "acme/api" when the apps came from a private
// GitLab is a picture of the wrong system.
func (i RepoIdentity) String() string {
	switch {
	case !i.Set():
		return ""
	case i.Host == "":
		return i.Path()
	default:
		return i.Host + "/" + i.Path()
	}
}

// Key is what decides whether two apps came from one repository.
//
// Lowercased, because a forge that treats "harehaDET/App" and "harehadet/app"
// as one repository would otherwise get two projects — one per spelling
// somebody happened to paste — and the grouping would be worse than none.
func (i RepoIdentity) Key() string { return strings.ToLower(i.String()) }

// Identity reads the repository's identity out of its URL.
//
// Lenient where Validate is strict: this runs against URLs already stored,
// including any a stricter Validate would refuse today, and an identity it
// cannot read is the zero value rather than an error. Nothing here decides
// whether a clone will work — only what to call the group.
func (r Repo) Identity() RepoIdentity {
	raw := strings.TrimSpace(r.URL)
	if raw == "" {
		return RepoIdentity{}
	}

	// scp-style "git@github.com:owner/name.git" is not a URL and url.Parse
	// reads the whole thing as an opaque path. Validate refuses it, so it
	// cannot be stored by this version — but an install that predates that
	// check can still be holding one.
	if !strings.Contains(raw, "://") {
		if host, path, ok := strings.Cut(raw, ":"); ok {
			raw = "ssh://" + host + "/" + path
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return RepoIdentity{}
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	// A trailing ".git" is a transport detail, not part of the name.
	last := strings.TrimSuffix(segments[len(segments)-1], ".git")
	// Nothing nameable is the zero value, not a group called ":". The identity
	// is only ever used as a label, and a label with no letter or digit in it
	// tells a reader less than no label at all.
	if !strings.ContainsFunc(last, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}) {
		return RepoIdentity{}
	}

	return RepoIdentity{
		Host:  u.Hostname(),
		Owner: strings.Join(segments[:len(segments)-1], "/"),
		Name:  last,
	}
}
