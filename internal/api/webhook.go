package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/kingzion24/ozymandis/internal/app"
)

// Webhooks is the delivery surface.
type Webhooks interface {
	HandlePush(ctx context.Context, body []byte, signature, appID string) (app.WebhookResult, error)
}

// maxWebhookBody caps a delivery.
//
// GitHub's own limit is 25 MB and a push payload is normally a few kilobytes;
// this is generous for the real thing and small enough that an unauthenticated
// endpoint cannot be used to spend this process's memory. It has to be
// generous-but-bounded rather than unbounded precisely because the signature
// cannot be checked until the whole body has been read — there is no way to
// authenticate first.
const maxWebhookBody = 5 << 20 // 5 MiB

// WebhookHandler serves GitHub deliveries.
//
// Mounted OUTSIDE the identity middleware, because GitHub carries no credential
// of ours. The signature is the authentication, and it is the only one — which
// is why the body limit and the constant-time comparison are not optional
// details on this path.
func WebhookHandler(hooks Webhooks, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Recoverer)

	h := &webhookServer{hooks: hooks, log: log}

	// Both shapes. The per-app URL is the tidy one; the bare URL is what makes
	// a single hook configurable across a whole organisation, and it works
	// because the signature — not the URL — selects the app.
	r.Post("/webhooks/github", h.push)
	r.Post("/webhooks/github/{app}", h.push)

	return r
}

type webhookServer struct {
	hooks Webhooks
	log   *slog.Logger
}

func (s *webhookServer) push(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")

	// A ping is what GitHub sends when a hook is created. Answering it is how
	// the person configuring it sees a green tick instead of a red one, and it
	// carries no push to act on.
	if event == "ping" {
		writeJSON(w, s.log, http.StatusOK, map[string]string{"ok": "pong"})
		return
	}
	if event != "push" {
		// Other events are not an error — a repository may deliver several —
		// so this is an acknowledgement rather than a refusal. Refusing would
		// show up in the hook's delivery log as a failure the person then has
		// to investigate.
		writeJSON(w, s.log, http.StatusOK,
			map[string]string{"ignored": event})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "could not read the delivery")
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	appID := chi.URLParam(r, "app")

	// Detached from the request, and this is the point of the endpoint's
	// design: a deploy takes minutes and GitHub gives a webhook ten seconds.
	// Holding the request open would show every real deploy in the delivery log
	// as a timeout, and GitHub would retry it — starting a second build of the
	// same commit.
	//
	// Bounded rather than merely detached, for the reason every detached write
	// in this codebase is: a context nothing can cancel is a goroutine nothing
	// can end.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)

	done := make(chan app.WebhookResult, 1)
	errc := make(chan error, 1)
	go func() {
		defer cancel()
		res, err := s.hooks.HandlePush(ctx, body, signature, appID)
		if err != nil {
			errc <- err
			return
		}
		done <- res
	}()

	// Waited on only long enough to answer honestly about the SIGNATURE, which
	// is fast — it is an HMAC over a few kilobytes. What is not waited on is the
	// deploy, which the service backgrounds itself.
	select {
	case res := <-done:
		if res.Deployed {
			writeJSON(w, s.log, http.StatusAccepted, map[string]any{
				"app": res.AppName, "deploying": true, "reason": res.Reason,
			})
			return
		}
		// A delivery that verified and correctly declined. 200, not an error:
		// in a monorepo this is the common case, and a hook whose delivery log
		// is full of red for working correctly is a hook somebody turns off.
		writeJSON(w, s.log, http.StatusOK, map[string]any{
			"app": res.AppName, "deploying": false, "reason": res.Reason,
		})

	case err := <-errc:
		if errors.Is(err, app.ErrNoMatchingApp) {
			// One answer for an unknown app and a bad signature alike. Telling
			// them apart would let anybody probe which app ids exist by watching
			// which ones answer differently.
			writeError(w, http.StatusUnauthorized, CodeUnauthenticated,
				"this delivery is not signed by any app's secret")
			return
		}
		writeServiceError(w, s.log, "handle push", err)

	case <-time.After(10 * time.Second):
		// The signature check should take microseconds; something is very
		// wrong. Accepted rather than failed, because the work is already
		// running detached and telling GitHub it failed would earn a retry.
		writeJSON(w, s.log, http.StatusAccepted,
			map[string]any{"deploying": true, "reason": "still working"})
	}
}
