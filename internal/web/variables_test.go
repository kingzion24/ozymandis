package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/app"
)

// varsApp is an app carrying one sealed credential and one plain setting,
// which is the shape every real app here has.
func varsApp(owner, name string) app.App {
	a := sampleApp(owner, name)
	a.Variables = []app.Variable{
		{Key: "ANTHROPIC_API_KEY", Secret: true},
		{Key: "LOG_LEVEL", Value: "info"},
	}
	return a
}

// Rotating a credential must not go through Remove.
//
// The form was always an upsert, so this was possible before the control
// existed — but the only thing offered beside a row was Remove, and removing a
// variable to retype it takes it away from a running app for as long as the
// typing takes. The control has to be there for the safe path to be the
// obvious one.
func TestAVariableCanBeEditedWithoutRemovingItFirst(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(varsApp("owner-1", "web"))})

	body := get(t, h, "/apps/web/variables").Body.String()
	if !strings.Contains(body, "/apps/web/variables?edit=ANTHROPIC_API_KEY") {
		t.Error("no Edit control beside the variable rows")
	}
}

func TestEditingOpensTheFormOnThatVariable(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(varsApp("owner-1", "web"))})

	body := get(t, h, "/apps/web/variables?edit=ANTHROPIC_API_KEY").Body.String()

	if !strings.Contains(body, "Replace ANTHROPIC_API_KEY") {
		t.Error("the form does not say which variable it is replacing")
	}
	// Read-only rather than absent: typing over the key would create a second
	// variable and leave the first one set on a running app.
	if !strings.Contains(body, `readonly value="ANTHROPIC_API_KEY"`) {
		t.Error("the key is not carried into the form read-only")
	}
	// The Secret box has to arrive already ticked. Left clear, saving would
	// quietly turn a sealed credential into a readable one.
	// Matched on the single-key form's own checkbox: the paste box below it
	// also has a ticked Secret control, and an assertion that could not tell
	// them apart would pass with this behaviour removed.
	if !strings.Contains(body, `id="var-secret" name="secret" value="1" checked`) {
		t.Error("the Secret box did not carry over from the variable being edited")
	}
}

// A sealed value has no read path anywhere in this codebase, and a form is
// exactly where that rule would be broken for convenience.
func TestEditingNeverPutsTheOldValueOnScreen(t *testing.T) {
	a := varsApp("owner-1", "web")
	a.Variables[1] = app.Variable{Key: "LOG_LEVEL", Value: "s3cret-looking"}
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web/variables?edit=LOG_LEVEL").Body.String()
	if strings.Contains(body, `value="s3cret-looking"`) {
		t.Error("the form prefilled the existing value into the value input")
	}
}

// ?edit= is whatever somebody typed. A form that opened on a key the app does
// not have would offer to "replace" something it was about to create, with the
// Secret box guessed rather than carried over.
func TestEditingAKeyTheAppDoesNotHaveDrawsTheOrdinaryForm(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(varsApp("owner-1", "web"))})

	body := get(t, h, "/apps/web/variables?edit=NOT_A_KEY").Body.String()
	if strings.Contains(body, "Replace NOT_A_KEY") {
		t.Error("the form opened on a variable the app does not have")
	}
	if !strings.Contains(body, "Set a variable") {
		t.Error("expected the ordinary add form")
	}
}

func TestAPastedEnvSetsEveryVariableInIt(t *testing.T) {
	apps := newFakeApps(varsApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/variables/import", url.Values{
		"env":    {"# rotated today\nANTHROPIC_API_KEY=sk-ant-new\n\nDEEPSEEK_MODEL=deepseek-chat\n"},
		"secret": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST import = %d, want 303: %s", rec.Code, rec.Body)
	}

	if len(apps.variables) != 2 {
		t.Fatalf("set %d variables, want 2: %v", len(apps.variables), apps.variables)
	}
	// Sorted, so a batch that fails partway fails at the same key every time
	// rather than wherever map iteration happened to reach.
	if apps.variables[0].Key != "ANTHROPIC_API_KEY" || apps.variables[1].Key != "DEEPSEEK_MODEL" {
		t.Errorf("keys arrived as %v, want them sorted", apps.variables)
	}
	if got := apps.variables[0].Value; got != "sk-ant-new" {
		t.Errorf("value = %q, want %q", got, "sk-ant-new")
	}
	for _, v := range apps.variables {
		if !v.Secret {
			t.Errorf("%s was not sealed", v.Key)
		}
	}
}

// Unticking Secret has to reach the service, or a log level pasted in bulk
// becomes an unreadable sealed value that needs the secret key to change.
func TestAPastedEnvCanBeStoredReadable(t *testing.T) {
	apps := newFakeApps(varsApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	post(t, h, "/apps/web/variables/import", url.Values{"env": {"LOG_LEVEL=debug"}})

	if len(apps.variables) != 1 || apps.variables[0].Secret {
		t.Errorf("stored %v, want one unsealed variable", apps.variables)
	}
}

// The whole value of pasting a block is not reading it line by line.
func TestABadLineInAPasteSaysWhichLineAndWritesNothing(t *testing.T) {
	apps := newFakeApps(varsApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/variables/import", url.Values{
		"env": {"A=1\nthis is prose\nB=2"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST import = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "line 2") {
		t.Errorf("the refusal does not say which line: %s", rec.Body)
	}
	// Parsing happens before any write, so a paste with a typo in it leaves the
	// app exactly as it was rather than half-updated.
	if len(apps.variables) != 0 {
		t.Errorf("wrote %v before refusing", apps.variables)
	}
}

// There is no transaction spanning a batch. Claiming otherwise would be worse
// than saying where it stopped, so the refusal names the key.
func TestAPasteThatFailsPartwayNamesTheKeyItStoppedAt(t *testing.T) {
	apps := newFakeApps(varsApp("owner-1", "web"))
	apps.varErr = "B_KEY"
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/variables/import", url.Values{
		"env": {"A_KEY=1\nB_KEY=2\nC_KEY=3"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a failing batch reported success")
	}
	if !strings.Contains(rec.Body.String(), "B_KEY") {
		t.Errorf("the refusal does not name the key it stopped at: %s", rec.Body)
	}
	if len(apps.variables) != 1 || apps.variables[0].Key != "A_KEY" {
		t.Errorf("wrote %v, want only the key before the failure", apps.variables)
	}
}

func TestAnEmptyPasteIsRefusedRatherThanRedirecting(t *testing.T) {
	apps := newFakeApps(varsApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps/web/variables/import", url.Values{"env": {"\n  \n# nothing\n"}})
	if rec.Code == http.StatusSeeOther {
		t.Error("an empty paste reported success")
	}
}
