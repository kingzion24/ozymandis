package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrNoSecretKeyForDeployKey means this install cannot hold a deploy key.
var ErrNoSecretKeyForDeployKey = errors.New(
	"app: no OZYMANDIS_SECRET_KEY is configured, so a deploy key cannot be stored safely")

// DeployKey is the pair that clones a private repository.
type DeployKey struct {
	// Public is the line to paste into the repository's deploy-key settings.
	Public string

	// Private is never returned. The field does not exist for a reason: the
	// sealed half goes from the database to a build's Secret and nowhere else,
	// and a struct with somewhere to put it is a struct somebody will log.
}

// GenerateDeployKey mints a key pair for an app and returns the public half.
//
// ed25519 rather than RSA: shorter, faster to verify, and accepted by every
// host worth naming. The comment carries the app name so a person looking at a
// repository's deploy keys months later can tell which app it belongs to —
// GitHub shows the comment and nothing else useful.
//
// Generated here rather than pasted in, deliberately. A key somebody supplies
// is usually one they already had, which usually means it opens every
// repository they can see; a key minted per app opens exactly one, and the
// private half exists in exactly one place.
//
// Regenerating replaces the old pair. The old public key stops working the
// moment the repository is updated, which is the point — a leaked key is
// revoked by minting another and swapping it.
func (s *Service) GenerateDeployKey(
	ctx context.Context, ownerID, name string,
) (DeployKey, error) {
	if !s.keeper.Configured() {
		return DeployKey{}, ErrNoSecretKeyForDeployKey
	}

	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return DeployKey{}, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return DeployKey{}, fmt.Errorf("app: generate deploy key: %w", err)
	}

	comment := "ozymandis-" + a.Name
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return DeployKey{}, fmt.Errorf("app: encode deploy key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return DeployKey{}, fmt.Errorf("app: encode public key: %w", err)
	}
	// The authorized_keys line, with the comment appended: what a host's
	// deploy-key form expects, verbatim.
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment

	sealed, err := s.keeper.Seal(string(pem.EncodeToMemory(block)))
	if err != nil {
		return DeployKey{}, fmt.Errorf("app: seal deploy key: %w", err)
	}

	if err := s.q.SetAppDeployKey(ctx, dbgen.SetAppDeployKeyParams{
		OwnerID: ownerID, ID: a.ID,
		DeployKey: sealed, DeployKeyPublic: public,
	}); err != nil {
		return DeployKey{}, fmt.Errorf("app: store deploy key: %w", err)
	}

	s.log.Info("deploy key generated", slog.String("app", a.Name))
	return DeployKey{Public: public}, nil
}

// deployKeyFor unseals the private half for a build.
//
// Read per build rather than held, so the key is unsealed only when something
// is about to clone with it — the same rule the registry credential follows.
// Returns nil when there is none, which is every app cloning over https from a
// public repository.
func (s *Service) deployKeyFor(ctx context.Context, ownerID string, a App) []byte {
	if !s.keeper.Configured() {
		return nil
	}

	row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: ownerID, ID: a.ID})
	if err != nil || len(row.DeployKey) == 0 {
		return nil
	}

	key, err := s.keeper.Open(row.DeployKey)
	if err != nil {
		// Named rather than silent: a key that cannot be opened means the
		// secret key changed, and the clone that is about to fail will say
		// "permission denied" with no hint why.
		s.log.Error("cannot open this app's deploy key",
			slog.String("app", a.Name), slog.String("error", err.Error()))
		return nil
	}
	return []byte(key)
}

// SetAutoDeploy turns deploy-on-push on or off.
//
// Switching ON mints a webhook secret and returns it ONCE. Only the sealed form
// is stored, so this return is the single moment it exists in readable form —
// the same rule an API token follows, and for the same reason: a database dump
// must not be a set of working webhook signatures.
//
// Switching OFF leaves the secret in place rather than clearing it. Somebody
// toggling this while debugging should not have to reconfigure their repository
// afterwards, and a secret whose app has auto-deploy off is inert: the candidate
// query filters on auto_deploy, so nothing will match it.
func (s *Service) SetAutoDeploy(
	ctx context.Context, ownerID, name string, on bool,
) (string, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return "", err
	}
	if !a.Repo.Set() {
		return "", errors.New(
			"app: deploy on push needs a repository — this app runs a prebuilt image")
	}

	row, err := s.q.SetAppAutoDeploy(ctx, dbgen.SetAppAutoDeployParams{
		OwnerID: ownerID, ID: a.ID, AutoDeploy: on,
	})
	if err != nil {
		return "", fmt.Errorf("app: set auto-deploy: %w", err)
	}

	if !on {
		s.log.Info("deploy on push disabled", slog.String("app", name))
		return "", nil
	}

	if !s.keeper.Configured() {
		return "", ErrNoSecretKey
	}

	// Minted only when there is not one already, so toggling off and on again
	// does not silently invalidate a webhook somebody has already configured.
	if len(row.WebhookSecret) > 0 {
		s.log.Info("deploy on push enabled with the existing secret",
			slog.String("app", name))
		return "", nil
	}

	raw, err := newWebhookSecret()
	if err != nil {
		return "", err
	}
	sealed, err := s.keeper.Seal(raw)
	if err != nil {
		return "", fmt.Errorf("app: seal the webhook secret: %w", err)
	}
	if err := s.q.SetAppWebhookSecret(ctx, dbgen.SetAppWebhookSecretParams{
		OwnerID: ownerID, ID: a.ID, WebhookSecret: sealed,
	}); err != nil {
		return "", fmt.Errorf("app: store the webhook secret: %w", err)
	}

	s.log.Info("deploy on push enabled", slog.String("app", name))
	return raw, nil
}

// newWebhookSecret mints the shared secret GitHub signs with.
//
// 32 bytes of randomness, hex-encoded: beyond guessing, and made of characters
// every webhook form accepts without anybody wondering about escaping.
func newWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("app: generate a webhook secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
