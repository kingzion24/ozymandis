package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/app"
)

// fakeHooks records what it was handed.
type fakeHooks struct {
	body      []byte
	signature string
	appID     string
	calls     int

	res app.WebhookResult
	err error
}

func (f *fakeHooks) HandlePush(
	_ context.Context, body []byte, signature, appID string,
) (app.WebhookResult, error) {
	f.calls++
	f.body, f.signature, f.appID = body, signature, appID
	return f.res, f.err
}

func deliver(h http.Handler, event, sig, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A ping is what GitHub sends when a hook is created, and answering it is how
// the person configuring it sees a green tick.
func TestAPingIsAnswered(t *testing.T) {
	hooks := &fakeHooks{}
	h := WebhookHandler(hooks, quiet())

	w := deliver(h, "ping", "", "/webhooks/github", `{"zen":"..."}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if hooks.calls != 0 {
		t.Error("a ping was treated as a push")
	}
}

// Other events are acknowledged rather than refused. A hook whose delivery log
// fills with red for working correctly is a hook somebody turns off.
func TestAnUnrelatedEventIsAcknowledged(t *testing.T) {
	hooks := &fakeHooks{}
	h := WebhookHandler(hooks, quiet())

	w := deliver(h, "issues", "", "/webhooks/github", `{}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — refusing shows as a failed delivery", w.Code)
	}
	if hooks.calls != 0 {
		t.Error("an issues event was handled as a push")
	}
}

// A delivery nobody can sign for is 401, and says nothing about which apps
// exist — the same answer for an unknown app and a bad signature.
func TestAnUnsignedDeliveryIs401(t *testing.T) {
	hooks := &fakeHooks{err: app.ErrNoMatchingApp}
	h := WebhookHandler(hooks, quiet())

	w := deliver(h, "push", "sha256=deadbeef", "/webhooks/github", `{"ref":"refs/heads/main"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}

	body := w.Body.String()
	for _, leak := range []string{"app", "exist", "unknown"} {
		if strings.Contains(strings.ToLower(body), leak+" not found") {
			t.Errorf("the response distinguishes a missing app from a bad "+
				"signature, which lets somebody probe which apps exist: %s", body)
		}
	}
}

// A deploy that started is 202, not 200 — and it returns without waiting for
// the build. GitHub gives a webhook ten seconds and a build takes minutes; a
// handler that waited would show every real deploy as a timeout and earn a
// retry, which starts a second build of the same commit.
func TestAStartedDeployIs202(t *testing.T) {
	hooks := &fakeHooks{res: app.WebhookResult{
		AppName: "api", Deployed: true, Reason: "a push touching services/api/x.go",
	}}
	h := WebhookHandler(hooks, quiet())

	w := deliver(h, "push", "sha256=abc", "/webhooks/github", `{"ref":"refs/heads/main"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["app"] != "api" || body["deploying"] != true {
		t.Errorf("body = %v", body)
	}
}

// A delivery that verified and correctly DECLINED is 200 with the reason, not
// an error. In a monorepo this is the common case.
func TestADeclinedDeliveryIs200WithTheReason(t *testing.T) {
	hooks := &fakeHooks{res: app.WebhookResult{
		AppName: "api", Deployed: false,
		Reason: "no changed file is under services/api/",
	}}
	h := WebhookHandler(hooks, quiet())

	w := deliver(h, "push", "sha256=abc", "/webhooks/github", `{"ref":"refs/heads/main"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a correct decline is not a failure", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["deploying"] != false {
		t.Errorf("body = %v", body)
	}
	// The reason travels, so the delivery log says WHY nothing happened.
	if !strings.Contains(body["reason"].(string), "services/api") {
		t.Errorf("the reason was not carried: %v", body)
	}
}

// Both URL shapes work, and the per-app one passes its id through.
func TestBothURLShapes(t *testing.T) {
	t.Run("bare", func(t *testing.T) {
		hooks := &fakeHooks{res: app.WebhookResult{AppName: "api", Deployed: true}}
		h := WebhookHandler(hooks, quiet())

		deliver(h, "push", "sha256=abc", "/webhooks/github", `{}`)
		if hooks.appID != "" {
			t.Errorf("appID = %q, want empty for the bare URL", hooks.appID)
		}
	})

	t.Run("per app", func(t *testing.T) {
		hooks := &fakeHooks{res: app.WebhookResult{AppName: "api", Deployed: true}}
		h := WebhookHandler(hooks, quiet())

		deliver(h, "push", "sha256=abc", "/webhooks/github/abc-123", `{}`)
		if hooks.appID != "abc-123" {
			t.Errorf("appID = %q, want abc-123", hooks.appID)
		}
	})
}

// The body and signature reach the service unaltered — the signature is over
// the exact bytes, so any rewriting here breaks every delivery.
func TestTheBodyAndSignatureArePassedThroughVerbatim(t *testing.T) {
	hooks := &fakeHooks{res: app.WebhookResult{AppName: "api"}}
	h := WebhookHandler(hooks, quiet())

	const body = `{"ref":"refs/heads/main","after":"abc123"}`
	deliver(h, "push", "sha256=cafe", "/webhooks/github", body)

	if string(hooks.body) != body {
		t.Errorf("body = %q, want it byte-identical — the signature is over "+
			"these exact bytes", hooks.body)
	}
	if hooks.signature != "sha256=cafe" {
		t.Errorf("signature = %q", hooks.signature)
	}
}

// The body is bounded. The signature cannot be checked until the whole body is
// read, so there is no way to authenticate first — which makes the limit the
// only thing standing between an unauthenticated endpoint and this process's
// memory.
func TestTheBodyIsBounded(t *testing.T) {
	hooks := &fakeHooks{res: app.WebhookResult{AppName: "api"}}
	h := WebhookHandler(hooks, quiet())

	huge := strings.Repeat("a", maxWebhookBody+1024)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(huge))
	r.Header.Set("X-GitHub-Event", "push")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if len(hooks.body) > maxWebhookBody {
		t.Errorf("read %d bytes, want at most %d — an unauthenticated endpoint "+
			"with an unbounded read is a way to spend this process's memory",
			len(hooks.body), maxWebhookBody)
	}
}

// The handler must be reachable WITHOUT a credential — it is mounted outside
// the identity middleware, because GitHub carries none.
func TestTheWebhookNeedsNoCredential(t *testing.T) {
	hooks := &fakeHooks{res: app.WebhookResult{AppName: "api", Deployed: true}}
	h := WebhookHandler(hooks, quiet())

	// No Authorization header, no cookie.
	w := deliver(h, "push", "sha256=abc", "/webhooks/github", `{}`)
	if w.Code == http.StatusUnauthorized {
		t.Error("the webhook demanded a credential — GitHub has none to send, " +
			"so the hook would never fire")
	}
	if hooks.calls != 1 {
		t.Errorf("the service was called %d times, want 1", hooks.calls)
	}
}

var _ = io.Discard
