package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrNoMatchingApp means no app's secret verified the delivery.
//
// One error for "no such app" and "wrong signature" alike. Telling them apart
// would let anybody probe which app ids exist by watching which ones answer
// differently — the same reasoning that makes a stranger's app a 404 rather
// than a 403.
var ErrNoMatchingApp = errors.New("app: no app matched this delivery")

// WebhookResult is what a delivery did.
type WebhookResult struct {
	// AppName is the app that was matched and acted on, empty when none was.
	AppName string

	// Deployed reports whether a deployment was started, and Reason says why
	// not when it was not. A delivery that correctly declines is the common
	// case in a monorepo, and it must be distinguishable from one that failed.
	Deployed bool
	Reason   string
}

// HandlePush verifies a delivery and deploys the app it belongs to.
//
// # The signature selects the app, not the payload
//
// This is the whole security of the endpoint. A push payload carries a
// repository URL, and anybody can POST a body naming any repository — so the
// URL cannot be what decides which app is deployed. Instead every candidate
// app's secret is tried against the delivery, and the app whose secret VERIFIES
// is the one acted on.
//
// The difference matters most in the case that looks harmless: two apps
// tracking the same repository. Selecting by URL would find both and have to
// guess; selecting by signature finds exactly the one whose webhook this is.
// And an attacker who knows a repository URL learns nothing, because they
// cannot produce a signature for it.
//
// appID narrows the candidates when the delivery arrived at a per-app URL. It
// is a hint, not an authorisation: the signature is still checked, so a crafted
// id reaches an app it cannot sign for and is refused there.
func (s *Service) HandlePush(
	ctx context.Context, body []byte, signature, appID string,
) (WebhookResult, error) {
	candidates, err := s.q.ListAutoDeployApps(ctx)
	if err != nil {
		return WebhookResult{}, fmt.Errorf("app: list auto-deploy apps: %w", err)
	}

	for _, row := range candidates {
		if appID != "" && row.ID.String() != appID {
			continue
		}
		if len(row.WebhookSecret) == 0 {
			continue
		}

		secret, err := s.keeper.Open(row.WebhookSecret)
		if err != nil {
			// A secret this install cannot unseal is a configuration fault, not
			// a reason to treat the delivery as unsigned. Logged and skipped, so
			// one broken app does not make every other app's webhook fail.
			s.log.Error("cannot open a webhook secret",
				slog.String("app", row.Name), slog.String("error", err.Error()))
			continue
		}

		if err := VerifySignature([]byte(secret), body, signature); err != nil {
			continue
		}

		// Verified. THIS is the app, whatever the payload says about itself.
		return s.deployFromPush(ctx, toApp(row), body)
	}

	return WebhookResult{}, ErrNoMatchingApp
}

// deployFromPush applies the fan-out rules and starts a deploy if they pass.
func (s *Service) deployFromPush(
	ctx context.Context, a App, body []byte,
) (WebhookResult, error) {
	ev, err := ParsePush(body)
	if err != nil {
		return WebhookResult{}, fmt.Errorf("app: parse push: %w", err)
	}

	ok, reason := ShouldDeploy(a, ev)
	if !ok {
		s.log.Info("push did not deploy",
			slog.String("app", a.Name), slog.String("reason", reason))
		return WebhookResult{AppName: a.Name, Reason: reason}, nil
	}

	// Recorded BEFORE the deploy starts, so a redelivery arriving while the
	// first build is still running does not start a second one. The deploy is
	// backgrounded and takes minutes; GitHub's redelivery window is shorter
	// than that.
	if err := s.q.SetAppLastDeployedSHA(ctx, dbgen.SetAppLastDeployedSHAParams{
		OwnerID: a.OwnerID, ID: a.ID, LastDeployedSha: ev.After,
	}); err != nil {
		return WebhookResult{}, fmt.Errorf("app: record the deployed commit: %w", err)
	}

	if err := s.Redeploy(ctx, a.OwnerID, a.Name); err != nil {
		return WebhookResult{AppName: a.Name}, err
	}

	s.log.Info("push deployed",
		slog.String("app", a.Name), slog.String("commit", ev.After),
		slog.String("reason", reason))
	return WebhookResult{AppName: a.Name, Deployed: true, Reason: reason}, nil
}
