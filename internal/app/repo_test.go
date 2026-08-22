package app

import (
	"strings"
	"testing"
)

// A repository is cloned and then built and then run, so what is accepted here
// decides what this cluster executes.
func TestARepositoryIsRefusedWhenItIsNotOne(t *testing.T) {
	for _, tc := range []struct {
		repo Repo
		why  string
	}{
		{Repo{URL: "github.com/you/app"}, "no scheme"},
		{Repo{URL: "http://github.com/you/app"}, "plain http"},
		{Repo{URL: "file:///etc/passwd"}, "a local path"},
		{Repo{URL: "git://github.com/you/app"}, "an unauthenticated protocol"},
		{Repo{URL: "https:///no-host"}, "no host"},
		{Repo{URL: "https://github.com/you/app", Branch: "--upload-pack=sh"}, "a flag as a branch"},
		{Repo{URL: "https://github.com/you/app", Branch: "a b"}, "a space in the branch"},
		{Repo{URL: "https://github.com/you/app", Subdir: "../../etc"}, "traversal"},
		{Repo{URL: "https://github.com/you/app", Subdir: "a/../../b"}, "traversal in the middle"},
		{Repo{URL: "https://github.com/you/app", Subdir: "/absolute"}, "an absolute path"},
		{Repo{URL: "https://github.com/you/app", Subdir: `back\slash`}, "a backslash"},
	} {
		if err := tc.repo.Validate(); err == nil {
			t.Errorf("accepted %+v (%s)", tc.repo, tc.why)
		}
	}

	for _, repo := range []Repo{
		{URL: "https://github.com/you/app"},
		{URL: "https://github.com/you/app.git", Branch: "main"},
		{URL: "ssh://git@github.com/you/app.git", Branch: "release/1.2"},
		{URL: "https://gitlab.example.com/you/app", Subdir: "services/api"},
	} {
		if err := repo.Validate(); err != nil {
			t.Errorf("refused %+v: %v", repo, err)
		}
	}
}

// Plain HTTP gets its own reason.
//
// It is the one refusal here somebody will think is pedantic, so the message
// has to say what the risk actually is rather than restating the rule.
func TestPlainHTTPSaysWhy(t *testing.T) {
	err := Repo{URL: "http://github.com/you/app"}.Validate()
	if err == nil {
		t.Fatal("plain http was accepted")
	}
	if !strings.Contains(err.Error(), "replaced") {
		t.Errorf("the message does not say what the risk is: %v", err)
	}
}

// An empty repository is not an invalid one.
//
// Most apps are an image and have no repository. Validate is called on all of
// them, so a zero Repo has to pass.
func TestAnAppWithNoRepositoryValidates(t *testing.T) {
	if err := (Repo{}).Validate(); err != nil {
		t.Errorf("an app with no repository was refused: %v", err)
	}
	if (Repo{}).Set() {
		t.Error("an empty repository reports itself as set")
	}
}

// A branch nobody chose is the default one.
func TestTheRefFallsBackToTheDefaultBranch(t *testing.T) {
	if got := (Repo{URL: "https://x/y"}).Ref(); got != DefaultBranch {
		t.Errorf("Ref() = %q, want %q", got, DefaultBranch)
	}
	if got := (Repo{URL: "https://x/y", Branch: "next"}).Ref(); got != "next" {
		t.Errorf("Ref() = %q, want next", got)
	}
}

// Normalising happens before validating, or a trailing slash is a refusal.
func TestNormaliseTrimsWhatAPersonTyped(t *testing.T) {
	got := Repo{
		URL: "  https://github.com/you/app  ", Branch: " main ", Subdir: " /services/api/ ",
	}.Normalise()

	if got.URL != "https://github.com/you/app" || got.Branch != "main" {
		t.Errorf("not trimmed: %+v", got)
	}
	if got.Subdir != "services/api" {
		t.Errorf("subdir = %q, want services/api", got.Subdir)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a normalised repository was refused: %v", err)
	}
}

// The identity is what groups apps in the dashboard, so it has to survive the
// several ways one repository gets written down: cloned over ssh by one app and
// https by the next, with or without the .git, through a port, and in the
// scp-style form an older install may still be holding.
func TestRepoIdentityReadsOneRepositoryOutOfEveryWayOfWritingIt(t *testing.T) {
	for _, tc := range []struct {
		url                     string
		host, owner, name, full string
	}{
		{
			url:  "ssh://git@github.com/kingzion24/MD_chatbot.git",
			host: "github.com", owner: "kingzion24", name: "MD_chatbot",
			full: "github.com/kingzion24/MD_chatbot",
		},
		{
			url:  "https://github.com/kingzion24/MD_chatbot",
			host: "github.com", owner: "kingzion24", name: "MD_chatbot",
			full: "github.com/kingzion24/MD_chatbot",
		},
		{
			url:  "git@github.com:harehaDET/mali-daftari-dashboard.git",
			host: "github.com", owner: "harehaDET", name: "mali-daftari-dashboard",
			full: "github.com/harehaDET/mali-daftari-dashboard",
		},
		{
			// A port is not part of which repository this is.
			url:  "https://git.example.com:8443/team/api.git",
			host: "git.example.com", owner: "team", name: "api",
			full: "git.example.com/team/api",
		},
		{
			// A GitLab subgroup nests, and the whole path is what makes two
			// repositories ending in "api" different.
			url:  "https://gitlab.com/beta/group/api.git",
			host: "gitlab.com", owner: "beta/group", name: "api",
			full: "gitlab.com/beta/group/api",
		},
	} {
		got := Repo{URL: tc.url}.Identity()
		if got.Host != tc.host || got.Owner != tc.owner || got.Name != tc.name {
			t.Errorf("Repo{%q}.Identity() = %+v, want host=%q owner=%q name=%q",
				tc.url, got, tc.host, tc.owner, tc.name)
		}
		if got.String() != tc.full {
			t.Errorf("Repo{%q}.Identity().String() = %q, want %q", tc.url, got.String(), tc.full)
		}
	}
}

// The same repository written two ways is one group, and two repositories that
// merely end in the same word are not.
func TestRepoIdentityKeyIgnoresHowTheURLWasSpelled(t *testing.T) {
	ssh := Repo{URL: "ssh://git@github.com/KingZion24/MD_chatbot.git"}.Identity()
	https := Repo{URL: "https://github.com/kingzion24/md_chatbot"}.Identity()
	if ssh.Key() != https.Key() {
		t.Errorf("one repository split into two groups: %q and %q", ssh.Key(), https.Key())
	}

	acme := Repo{URL: "https://github.com/acme/api.git"}.Identity()
	beta := Repo{URL: "https://github.com/beta/api.git"}.Identity()
	if acme.Key() == beta.Key() {
		t.Errorf("two repositories collapsed into one group: %q", acme.Key())
	}
}

// An app that was never built from source has no identity, and neither has a
// URL with nothing nameable in it. Both are the zero value rather than an
// error: nothing here decides whether a clone would work, only what to call a
// group, and a heading is not worth failing a page over.
func TestRepoIdentityIsEmptyWhenThereIsNothingToName(t *testing.T) {
	for _, url := range []string{"", "   ", "https://github.com", "https://github.com/", "::"} {
		if got := (Repo{URL: url}).Identity(); got.Set() {
			t.Errorf("Repo{%q}.Identity() = %+v, want unset", url, got)
		}
	}
}
