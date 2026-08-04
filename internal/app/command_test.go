package app

import (
	"slices"
	"strings"
	"testing"
)

func TestAPlainCommandSplitsOnWhitespace(t *testing.T) {
	got, err := ParseCommand("python log_consumer.py")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"python", "log_consumer.py"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRunsOfWhitespaceAreOneSeparator(t *testing.T) {
	got, err := ParseCommand("  uvicorn\tapp:app \n --port  8000 ")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"uvicorn", "app:app", "--port", "8000"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAnEmptyCommandIsNoCommandRatherThanAnError(t *testing.T) {
	got, err := ParseCommand("   ")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want no arguments", got)
	}
}

// The case that makes quoting worth implementing: split on whitespace and the
// JSON becomes two arguments that each mean nothing.
func TestQuotesHoldAnArgumentTogether(t *testing.T) {
	got, err := ParseCommand(`uvicorn app:app --log-config '{"version": 1}'`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"uvicorn", "app:app", "--log-config", `{"version": 1}`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDoubleQuotesHoldAnArgumentTogether(t *testing.T) {
	got, err := ParseCommand(`sh -c "sleep 1 && exec app"`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"sh", "-c", "sleep 1 && exec app"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestQuotesJoinRatherThanDelimit(t *testing.T) {
	got, err := ParseCommand(`--flag="a b"c`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"--flag=a bc"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestABackslashEscapesTheNextCharacter(t *testing.T) {
	got, err := ParseCommand(`app --path /a\ b --quote \"`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"app", "--path", "/a b", "--quote", `"`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Single quotes are the reliable way to pass a backslash through, so inside
// them it must stay an ordinary character.
func TestABackslashIsLiteralInsideSingleQuotes(t *testing.T) {
	got, err := ParseCommand(`app 'C:\logs\out'`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"app", `C:\logs\out`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAnEmptyQuotedArgumentSurvives(t *testing.T) {
	got, err := ParseCommand(`app --tag "" last`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"app", "--tag", "", "last"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAnUnclosedQuoteIsRefused(t *testing.T) {
	for _, line := range []string{`app "unclosed`, `app 'unclosed`} {
		if _, err := ParseCommand(line); err == nil {
			t.Fatalf("ParseCommand(%q) = nil error, want a refusal", line)
		}
	}
}

func TestATrailingBackslashIsRefused(t *testing.T) {
	if _, err := ParseCommand(`app --flag \`); err == nil {
		t.Fatal("ParseCommand = nil error, want a refusal")
	}
}

// An argv whose first element is empty names no program, and Kubernetes reports
// that as a fault of the platform rather than of the input.
func TestACommandStartingWithAnEmptyArgumentIsRefused(t *testing.T) {
	if _, err := ParseCommand(`"" server`); err == nil {
		t.Fatal("ParseCommand = nil error, want a refusal")
	}
}

func TestTooManyArgumentsAreRefused(t *testing.T) {
	line := "app" + strings.Repeat(" x", maxCommandArgs)
	if _, err := ParseCommand(line); err == nil {
		t.Fatalf("ParseCommand with %d arguments = nil error, want a refusal",
			maxCommandArgs+1)
	}
}

// No shell runs between the argv and the process, so a variable reference has
// to arrive at the process unexpanded rather than as an empty string.
func TestNoVariableExpansionHappens(t *testing.T) {
	got, err := ParseCommand(`app --home $HOME`)
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	want := []string{"app", "--home", "$HOME"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
