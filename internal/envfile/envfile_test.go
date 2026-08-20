package envfile

import (
	"strings"
	"testing"
)

func TestAPastedBlockBecomesVariables(t *testing.T) {
	got, err := Parse(strings.NewReader(strings.Join([]string{
		"# the model to use",
		"DEEPSEEK_MODEL=deepseek-chat",
		"",
		"  DEEPSEEK_ENABLED = true  ",
		"export ANTHROPIC_API_KEY=sk-ant-xyz",
		"DATABASE_URL=postgres://u:p@h:5432/db?sslmode=require",
	}, "\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := map[string]string{
		"DEEPSEEK_MODEL":    "deepseek-chat",
		"DEEPSEEK_ENABLED":  "true",
		"ANTHROPIC_API_KEY": "sk-ant-xyz",
		// The = inside a connection string belongs to the value. Cut splits on
		// the first one, which is the only reading that leaves this intact.
		"DATABASE_URL": "postgres://u:p@h:5432/db?sslmode=require",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d variables, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// A textarea submits CRLF, so this is what a browser paste actually looks like.
// Without the trim every value would end in a carriage return, and a credential
// that fails to authenticate for a reason you cannot see in the box you typed
// it into is the worst version of this bug.
func TestABrowserPasteDoesNotCarryItsLineEndings(t *testing.T) {
	got, err := Parse(strings.NewReader("A=1\r\nB=2\r\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for k, want := range map[string]string{"A": "1", "B": "2"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestALineThatIsNotAVariableSaysWhichOne(t *testing.T) {
	_, err := Parse(strings.NewReader("A=1\n\nthis is prose\nB=2\n"))
	if err == nil {
		t.Fatal("a line with no = was accepted")
	}
	// The line number is the whole value of the error: a paste is not read
	// line by line, so "it is wrong somewhere" is not an answer.
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error does not say which line: %v", err)
	}
}

func TestNothingIsWrittenForAnEmptyPaste(t *testing.T) {
	got, err := Parse(strings.NewReader("\n  \n# only a comment\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %v, want nothing", got)
	}
}
