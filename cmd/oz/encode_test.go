package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/appspec"
)

// The property `oz config --write` exists to have: what it writes, `oz deploy`
// can read.
//
// This failed on the first attempt, and in a way no unit test of either side
// would have caught. JSON has one number type, so every integer arrives from
// the API as a float64; encoding that map straight to TOML wrote
// `replicas = 2.0` and `port = 8080.0`, which the decoder then refused because
// the spec's fields are integers. The file the tool wrote was one the tool
// could not read.
func TestEncodedConfigParsesBack(t *testing.T) {
	// Exactly what the API sends: a JSON object, decoded generically.
	const fromServer = `{
		"name": "web",
		"build": {"image": "nginx:alpine"},
		"service": {"port": 8080, "internal": false},
		"health": {"path": "/healthz", "liveness": true},
		"scale": {"replicas": 2},
		"env": {"LOG_LEVEL": "info"}
	}`

	var raw map[string]any
	if err := json.Unmarshal([]byte(fromServer), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out, err := encodeSpec(raw)
	if err != nil {
		t.Fatalf("encodeSpec: %v", err)
	}

	// The specific corruption, named so a failure says what happened.
	if strings.Contains(string(out), "8080.0") || strings.Contains(string(out), "2.0") {
		t.Errorf("integers were written as floats:\n%s", out)
	}

	spec, err := appspec.Parse(out)
	if err != nil {
		t.Fatalf("the file this tool wrote does not parse with this tool: %v\n%s", err, out)
	}

	if spec.Name != "web" {
		t.Errorf("name = %q", spec.Name)
	}
	if spec.Service == nil || spec.Service.Port == nil || *spec.Service.Port != 8080 {
		t.Errorf("port did not survive: %+v", spec.Service)
	}
	if spec.Scale == nil || spec.Scale.Replicas == nil || *spec.Scale.Replicas != 2 {
		t.Errorf("replicas did not survive: %+v", spec.Scale)
	}
	if spec.Health == nil || spec.Health.Path == nil || *spec.Health.Path != "/healthz" {
		t.Errorf("health did not survive: %+v", spec.Health)
	}
	if spec.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env did not survive: %v", spec.Env)
	}
}

// Absent stays absent through the whole path: API JSON → Spec → TOML → Spec.
//
// The same absent-versus-zero property the appspec package is built around,
// checked at the one place it crosses three encodings. A config written for an
// app with no health probe must not acquire `path = ""`, which would tell the
// next converge to remove a probe nobody set.
func TestEncodedConfigKeepsAbsentAbsent(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"name":"web","scale":{"replicas":0}}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out, err := encodeSpec(raw)
	if err != nil {
		t.Fatalf("encodeSpec: %v", err)
	}
	if strings.Contains(string(out), "health") {
		t.Errorf("a section the server did not send was invented:\n%s", out)
	}

	spec, err := appspec.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if spec.Health != nil {
		t.Errorf("health = %+v, want nil", spec.Health)
	}
	// And an explicit zero survives as one rather than vanishing.
	if spec.Scale == nil || spec.Scale.Replicas == nil || *spec.Scale.Replicas != 0 {
		t.Errorf("an explicit `replicas = 0` did not survive: %+v", spec.Scale)
	}
}

// Strictness belongs to the WRITE path only.
//
// encodeSpec refuses unknown fields because it produces a file somebody
// commits, and silently dropping a setting a newer server added is worse than
// saying "upgrade oz". But that reasoning does not extend to reading: a field
// this build only renders, or does not render at all, must not stop `oz status`
// working against a server one version ahead.
//
// The asymmetry is easy to lose. Somebody tidying `Client.do` would reasonably
// add DisallowUnknownFields "for consistency" and turn every additive server
// change into a broken CLI for everyone who has not upgraded yet. This pins the
// split so that change fails here instead of in the field.
func TestOnlyTheWritePathIsStrictAboutUnknownFields(t *testing.T) {
	// A response from a server that knows about things this build does not.
	t.Run("app survives", func(t *testing.T) {
		var a App
		err := json.Unmarshal([]byte(
			`{"name":"web","port":8080,"region":"eu","autoscale":{"min":1}}`), &a)
		if err != nil {
			t.Fatalf("oz status broke against a newer server: %v", err)
		}
		if a.Name != "web" || a.Port != 8080 {
			t.Errorf("known fields did not survive: %+v", a)
		}
	})

	t.Run("deployment survives", func(t *testing.T) {
		var d Deployment
		err := json.Unmarshal([]byte(
			`{"id":"x","status":"active","finished":true,"release_status":"skipped"}`), &d)
		if err != nil {
			t.Fatalf("oz releases broke against a newer server: %v", err)
		}
		if d.Status != "active" || !d.Finished {
			t.Errorf("known fields did not survive: %+v", d)
		}
	})

	t.Run("config result survives", func(t *testing.T) {
		var r ConfigResult
		err := json.Unmarshal([]byte(
			`{"changes":[{"field":"a","skipped":true,"reason":"r","severity":"warn"}],"future":1}`), &r)
		if err != nil {
			t.Fatalf("oz deploy broke against a newer server: %v", err)
		}
		// And the part the deploy report depends on is intact.
		if len(r.Changes) != 1 || !r.Changes[0].Skipped || r.Changes[0].Reason != "r" {
			t.Errorf("the skip did not survive: %+v", r.Changes)
		}
	})

	t.Run("write path is strict", func(t *testing.T) {
		var raw map[string]any
		if err := json.Unmarshal([]byte(`{"name":"web","autoscale":{"min":1}}`), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, err := encodeSpec(raw); err == nil {
			t.Error("the write path silently dropped a section a newer server sent")
		}
	})
}

// A field a newer server knows about is caught with an explanation, not
// silently dropped into a file somebody is about to commit.
func TestEncodedConfigRefusesUnknownFields(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"name":"web","quantum":{"entangled":true}}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	_, err := encodeSpec(raw)
	if err == nil {
		t.Fatal("an unknown section was silently dropped")
	}
	if !strings.Contains(err.Error(), "Upgrade oz") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}
