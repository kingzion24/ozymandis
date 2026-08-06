package app

import "testing"

// The poll's output is parsed rather than trusted, because one unreachable
// repository must not stop the others being checked.
//
// git ls-remote prints nothing for a host it cannot reach, so the script emits
// an empty sha for that app and a real one for the rest. A parser that failed
// on the empty line would let one dead remote block every other app's deploys
// for as long as it stayed dead.
func TestParseLsRemoteSkipsWhatItCannotRead(t *testing.T) {
	out := "api\tabc123\n" +
		"broken\t\n" + // unreachable: no sha
		"web\tdef456\n" +
		"\n" + // blank line from the script
		"  spaced\t789abc  \n"

	heads := parseLsRemote(out)

	if heads["api"] != "abc123" || heads["web"] != "def456" {
		t.Errorf("heads = %v", heads)
	}
	if _, ok := heads["broken"]; ok {
		t.Error("an unreachable repository was reported as having a head")
	}
	if heads["spaced"] != "789abc" {
		t.Errorf("a padded line was not trimmed: %q", heads["spaced"])
	}
	if len(heads) != 3 {
		t.Errorf("got %d heads, want 3: %v", len(heads), heads)
	}
}

// App names become shell variable names, and the characters legal in one are
// not legal in the other.
func TestEnvKey(t *testing.T) {
	cases := map[string]string{
		"api":         "API",
		"my-app":      "MY_APP",
		"api.staging": "API_STAGING",
	}
	for in, want := range cases {
		if got := envKey(in); got != want {
			t.Errorf("envKey(%q) = %q, want %q", in, got, want)
		}
	}
}
