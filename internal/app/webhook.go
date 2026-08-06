package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// PushEvent is the part of a GitHub push payload this engine reads.
//
// Deliberately small. Everything else in a push delivery is either irrelevant
// or attacker-controlled in a way that must not influence a decision — see
// the note on Repository.
type PushEvent struct {
	// Ref is "refs/heads/main". Compared against the app's branch.
	Ref string `json:"ref"`

	// After is the commit now at the tip. "Deleted" pushes carry all zeroes.
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`

	// Repository is read for logging and for the bare-URL endpoint's candidate
	// lookup, and for NOTHING that decides whether to deploy. Anybody can POST
	// a body naming any repository; the signature is what proves which app a
	// delivery is for.
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
	} `json:"repository"`

	// Commits is what the push touched. A pointer to a slice, because ABSENT
	// and EMPTY are different and the difference decides whether a monorepo app
	// builds — see PathsTouched.
	Commits *[]PushCommit `json:"commits"`
}

// PushCommit is one commit's file lists.
type PushCommit struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

// ErrBadSignature means the delivery was not signed by the secret it claims.
var ErrBadSignature = errors.New("app: the webhook signature does not verify")

// VerifySignature checks a GitHub X-Hub-Signature-256 header against a secret.
//
// hmac.Equal rather than ==. A byte-by-byte string comparison returns as soon
// as it finds a difference, so how long it takes says how much of the prefix
// was right — and a signature is guessable one byte at a time by anybody who
// can measure that. hmac.Equal takes the same time whatever the inputs.
//
// The header carries a "sha256=" prefix. A missing or misspelled prefix is a
// refusal rather than something to be lenient about: the algorithm is part of
// what is being asserted, and accepting a bare hex digest would accept a
// signature computed with something weaker.
func VerifySignature(secret, body []byte, header string) error {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return ErrBadSignature
	}
	sent, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return ErrBadSignature
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(sent, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// ParsePush decodes a push delivery.
func ParsePush(body []byte) (PushEvent, error) {
	var ev PushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return PushEvent{}, err
	}
	return ev, nil
}

// Branch is the branch name a ref refers to, or empty for anything else.
//
// Tags and other refs are not branches, and an app tracks a branch. A tag push
// arriving as a deploy would ship whatever a release tag pointed at, on a
// schedule nobody chose.
func (e PushEvent) Branch() string {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(e.Ref, prefix) {
		return ""
	}
	return strings.TrimPrefix(e.Ref, prefix)
}

// PathsTouched returns every path the push touched, and whether the payload
// said at all.
//
// The second return is the whole of the fail-open policy. GitHub truncates the
// commit list on a large push and omits it entirely on a force-push, so an
// empty list means one of two very different things:
//
//	commits: [...]  — this is what was touched, believe it
//	commits absent  — GitHub did not say, and silence is not "nothing"
//
// A slice cannot express that difference, which is why PushEvent.Commits is a
// pointer: nil is "did not say" and an empty slice is "said, and it was
// nothing". Collapsing them would make every force-push look like a push that
// touched no files, and a monorepo app would silently stop deploying.
func (e PushEvent) PathsTouched() (paths []string, said bool) {
	if e.Commits == nil {
		return nil, false
	}
	for _, c := range *e.Commits {
		paths = append(paths, c.Added...)
		paths = append(paths, c.Modified...)
		paths = append(paths, c.Removed...)
	}
	return paths, true
}

// ShouldDeploy decides whether a push should deploy this app.
//
// A pure function of the event and the app, so the fan-out rules can be tested
// exhaustively without a database, a cluster, or an HTTP request. Every reason
// to decline is distinguishable in the returned string, because "nothing
// happened" is the hardest kind of bug to investigate and the reason is the
// only thing that makes it possible.
//
// # The monorepo rule, and why it fails OPEN
//
// Several apps can track one repository at different subdirs, and every one of
// them matches every push. Firing all of them on a one-line change to a single
// service would make a monorepo worse to host here than a set of small repos.
// So an app with a subdir deploys only when some touched path is under it.
//
// But the filter is best-effort in one direction ONLY. When GitHub does not say
// what was touched — truncated, or a force-push — the app deploys. A false
// rebuild costs a build; a false skip ships nothing and leaves somebody staring
// at a green dashboard showing the previous commit, with no error anywhere to
// explain it. The dangerous direction is the one that looks like success, which
// is exactly why it is the direction that must not be taken on incomplete
// information.
func ShouldDeploy(a App, e PushEvent) (bool, string) {
	if !a.AutoDeploy {
		return false, "auto-deploy is off for this app"
	}
	if !a.Repo.Set() {
		return false, "this app is not built from a repository"
	}
	if e.Deleted {
		return false, "the branch was deleted"
	}

	branch := e.Branch()
	if branch == "" {
		return false, "the ref is not a branch: " + e.Ref
	}
	if branch != a.Repo.Ref() {
		return false, "the push was to " + branch + ", this app tracks " + a.Repo.Ref()
	}

	if e.After == "" || isZeroSHA(e.After) {
		return false, "the push carries no commit"
	}
	if a.LastDeployedSHA != "" && e.After == a.LastDeployedSHA {
		// A redelivery, or somebody replaying one from the GitHub UI. Neither
		// is a reason to build the same commit again.
		return false, "this commit is already deployed"
	}

	// No subdir: the app builds from the repository root and depends on the
	// whole tree, so every push to its branch is its business.
	if a.Repo.Subdir == "" {
		return true, "a push to " + branch
	}

	paths, said := e.PathsTouched()
	if !said {
		// FAIL OPEN. See the doc comment.
		return true, "GitHub did not say which files changed, so this deploys " +
			"rather than risk skipping a real change"
	}
	for _, p := range paths {
		if underSubdir(p, a.Repo.Subdir) {
			return true, "a push touching " + p
		}
	}
	return false, "no changed file is under " + a.Repo.Subdir + "/"
}

// underSubdir reports whether a repository path lies inside a subdirectory.
//
// Boundary-aware: "services/api-v2/main.go" is NOT under "services/api", even
// though it has that prefix. A naive strings.HasPrefix would deploy the wrong
// service every time somebody added a sibling whose name extended another's,
// which in a monorepo is routine.
func underSubdir(path, subdir string) bool {
	subdir = strings.Trim(subdir, "/")
	if subdir == "" {
		return true
	}
	path = strings.TrimPrefix(path, "/")
	return path == subdir || strings.HasPrefix(path, subdir+"/")
}

// isZeroSHA reports the all-zeroes SHA a branch deletion carries.
func isZeroSHA(sha string) bool {
	return strings.Trim(sha, "0") == ""
}
