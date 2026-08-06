package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// gitApp is an app tracking a repository, with an optional subdir.
func gitApp(subdir string) App {
	return App{
		Name: "api", AutoDeploy: true,
		Repo: Repo{URL: "https://github.com/you/mono.git", Branch: "main", Subdir: subdir},
	}
}

// push builds an event. commits == nil means the payload did not say.
func push(sha string, commits *[]PushCommit) PushEvent {
	return PushEvent{Ref: "refs/heads/main", After: sha, Commits: commits}
}

func touched(paths ...string) *[]PushCommit {
	return &[]PushCommit{{Modified: paths}}
}

// --- The fail-open pair. Neither half alone proves anything. ---
//
// Fail-open means the DANGEROUS direction is the one that looks like success:
// an implementation that always fires ships every push and nothing looks wrong
// until a monorepo rebuilds twelve services on a typo fix. So the honest test
// is the one asserting a build that should NOT have fired didn't — and it only
// means something alongside its opposite.
//
//	always-fires  passes "no commit list deploys", fails "other service skipped"
//	always-skips  passes "other service skipped", fails "no commit list deploys"
//
// Only both together prove the filter reads the list when it is there and fails
// open when it is not.

// HALF ONE — the assertion against the thing that should not happen.
func TestAPushToAnotherServiceLeavesThisAppAlone(t *testing.T) {
	a := gitApp("services/api")

	ok, why := ShouldDeploy(a, push("abc123", touched("services/web/main.go")))

	if ok {
		t.Fatalf("a push touching only services/web deployed services/api — "+
			"every service in a monorepo would rebuild on every push (%s)", why)
	}
	if !strings.Contains(why, "services/api") {
		t.Errorf("the reason does not name the subdir that was checked: %q", why)
	}
}

// HALF TWO — the fail-safe direction.
func TestAPushWithNoCommitListDeploys(t *testing.T) {
	a := gitApp("services/api")

	// GitHub truncates the list on a large push and omits it on a force-push.
	ok, why := ShouldDeploy(a, push("abc123", nil))

	if !ok {
		t.Fatalf("a push with no commit list was skipped — a force-push would "+
			"silently stop deploying, leaving a green dashboard on a stale "+
			"commit with no error anywhere (%s)", why)
	}
	if !strings.Contains(why, "did not say") {
		t.Errorf("the reason does not explain the fail-open: %q", why)
	}
}

// And the third state the pointer exists to express: the payload SAID, and it
// said nothing was touched. That is not the same as not saying.
func TestAnEmptyCommitListIsNotAMissingOne(t *testing.T) {
	a := gitApp("services/api")

	empty := &[]PushCommit{}
	if ok, _ := ShouldDeploy(a, push("abc123", empty)); ok {
		t.Error("a push that said it touched nothing was treated as a push that " +
			"said nothing at all — the fail-open would fire on every empty push")
	}

	// While nil still deploys, so the two are genuinely distinguished.
	if ok, _ := ShouldDeploy(a, push("abc123", nil)); !ok {
		t.Error("a push that said nothing was skipped")
	}
}

// A push that DOES touch this service fires.
func TestAPushToThisServiceDeploys(t *testing.T) {
	a := gitApp("services/api")

	ok, why := ShouldDeploy(a, push("abc123", touched(
		"services/web/main.go", "services/api/handler.go")))
	if !ok {
		t.Fatalf("a push touching services/api did not deploy it (%s)", why)
	}
	if !strings.Contains(why, "services/api/handler.go") {
		t.Errorf("the reason does not name the file that matched: %q", why)
	}
}

// An app with no subdir builds from the root and depends on the whole tree.
func TestAnAppWithNoSubdirDeploysOnAnyPush(t *testing.T) {
	a := gitApp("")

	if ok, _ := ShouldDeploy(a, push("abc123", touched("anything/at/all.txt"))); !ok {
		t.Error("a root-built app skipped a push")
	}
}

// The prefix trap. "services/api-v2" is not under "services/api", and a naive
// HasPrefix deploys the wrong service every time somebody adds a sibling whose
// name extends another's — which in a monorepo is routine.
func TestSubdirMatchingRespectsPathBoundaries(t *testing.T) {
	a := gitApp("services/api")

	cases := map[string]bool{
		"services/api/main.go":    true,
		"services/api":            true,
		"services/api/deep/x.go":  true,
		"services/api-v2/main.go": false,
		"services/apifoo/main.go": false,
		"services/web/main.go":    false,
		"other/services/api/x.go": false,
	}
	for path, want := range cases {
		got, _ := ShouldDeploy(a, push("abc", touched(path)))
		if got != want {
			t.Errorf("%q under services/api = %v, want %v", path, got, want)
		}
	}
}

// --- Everything else that must not deploy ---

func TestPushesThatMustNotDeploy(t *testing.T) {
	cases := []struct {
		name string
		app  App
		ev   PushEvent
		want string
	}{
		{
			name: "auto-deploy off",
			app:  func() App { a := gitApp(""); a.AutoDeploy = false; return a }(),
			ev:   push("abc", touched("x")),
			want: "auto-deploy is off",
		},
		{
			name: "not a repository app",
			app:  App{Name: "web", AutoDeploy: true},
			ev:   push("abc", touched("x")),
			want: "not built from a repository",
		},
		{
			name: "a different branch",
			app:  gitApp(""),
			ev:   PushEvent{Ref: "refs/heads/feature", After: "abc", Commits: touched("x")},
			want: "this app tracks main",
		},
		{
			name: "a tag, not a branch",
			app:  gitApp(""),
			ev:   PushEvent{Ref: "refs/tags/v1.0.0", After: "abc", Commits: touched("x")},
			want: "not a branch",
		},
		{
			name: "a branch deletion",
			app:  gitApp(""),
			ev:   PushEvent{Ref: "refs/heads/main", After: "abc", Deleted: true},
			want: "deleted",
		},
		{
			name: "the all-zeroes SHA a deletion carries",
			app:  gitApp(""),
			ev:   push("0000000000000000000000000000000000000000", touched("x")),
			want: "no commit",
		},
		{
			name: "a commit already deployed",
			app: func() App {
				a := gitApp("")
				a.LastDeployedSHA = "abc123"
				return a
			}(),
			ev:   push("abc123", touched("x")),
			want: "already deployed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := ShouldDeploy(tc.app, tc.ev)
			if ok {
				t.Fatalf("deployed when it should not have (%s)", why)
			}
			if !strings.Contains(why, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", why, tc.want)
			}
		})
	}
}

// A redelivery must not rebuild, but a NEW commit must — the check is on the
// SHA, not on having deployed before.
func TestANewCommitDeploysEvenAfterOne(t *testing.T) {
	a := gitApp("")
	a.LastDeployedSHA = "old111"

	if ok, why := ShouldDeploy(a, push("new222", touched("x"))); !ok {
		t.Errorf("a new commit was skipped: %s", why)
	}
}

// --- HMAC: the security seam ---

// The probe that matters: a signature valid for app A, replayed against app B.
//
// The multi-app version of the lookalike-host row. Every candidate app is tried
// against the delivery, so a signature that verifies under A's secret must not
// verify under B's — otherwise one repository's webhook could deploy another
// team's app, and the payload's repository field cannot help because anybody
// can write anything there.
func TestASignatureForOneAppDoesNotVerifyForAnother(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	secretA := []byte("app-a-secret-aaaaaaaaaaaaaaaaaaaa")
	secretB := []byte("app-b-secret-bbbbbbbbbbbbbbbbbbbb")

	sigA := sign(secretA, body)

	if err := VerifySignature(secretA, body, sigA); err != nil {
		t.Fatalf("a signature did not verify under its own secret: %v", err)
	}
	if err := VerifySignature(secretB, body, sigA); !errors.Is(err, ErrBadSignature) {
		t.Errorf("app A's signature verified under app B's secret (err = %v) — "+
			"one repository's webhook could deploy another team's app", err)
	}
}

func TestSignatureVerification(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := []byte("a-secret")
	good := sign(secret, body)

	cases := map[string]string{
		"no prefix":          strings.TrimPrefix(good, "sha256="),
		"wrong algorithm":    strings.Replace(good, "sha256=", "sha1=", 1),
		"not hex":            "sha256=zzzz",
		"empty":              "",
		"truncated":          good[:len(good)-2],
		"one byte different": good[:len(good)-1] + flipHex(good[len(good)-1:]),
	}
	for name, header := range cases {
		if err := VerifySignature(secret, body, header); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}

	if err := VerifySignature(secret, body, good); err != nil {
		t.Errorf("a good signature was refused: %v", err)
	}

	// A body changed after signing must not verify — that is the point.
	if err := VerifySignature(secret, []byte(`{"ref":"refs/heads/evil"}`), good); err == nil {
		t.Error("a tampered body verified")
	}
}

func flipHex(s string) string {
	if s == "0" {
		return "1"
	}
	return "0"
}

func TestParsePush(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "abc123",
		"repository": {"full_name": "you/mono", "clone_url": "https://github.com/you/mono.git"},
		"commits": [{"added": ["a.go"], "modified": ["b.go"], "removed": ["c.go"]}]
	}`)

	ev, err := ParsePush(body)
	if err != nil {
		t.Fatalf("ParsePush: %v", err)
	}
	if ev.Branch() != "main" || ev.After != "abc123" {
		t.Errorf("event = %+v", ev)
	}
	paths, said := ev.PathsTouched()
	if !said || len(paths) != 3 {
		t.Errorf("paths = %v said = %v, want three paths", paths, said)
	}

	// And a payload with no commits key at all reports that it did not say.
	bare, err := ParsePush([]byte(`{"ref":"refs/heads/main","after":"x"}`))
	if err != nil {
		t.Fatalf("ParsePush: %v", err)
	}
	if _, said := bare.PathsTouched(); said {
		t.Error("a payload with no commits key claimed to have said what changed")
	}
}
