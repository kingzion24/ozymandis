package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kingzion24/ozymandis/internal/identity"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrTokenNotValid is returned for every API token that cannot be used.
//
// One error for unknown, expired, revoked and malformed alike, for the reason
// ErrSessionInvalid gives: a caller that could tell them apart could tell a
// real token from a guess, and so could anyone probing the endpoint that
// surfaces the difference.
var ErrTokenNotValid = errors.New("account: api token is not valid")

// ErrTokenNameTaken means this person already has a token by that name in this
// team.
var ErrTokenNameTaken = errors.New("account: a token with that name already exists")

// TokenPrefix marks a raw API token.
//
// Present so that a token pasted into a log, an issue, or a CI variable is
// recognisable as one without having to be checked against the database. The
// practical payoff is scanning: a fixed prefix is what lets `git grep`, a
// pre-commit hook, or GitHub's own secret scanning find a leaked credential,
// and a 43-character base64 string with no prefix is indistinguishable from
// every other opaque value in a config file.
const TokenPrefix = "oz_"

// APIToken is a credential someone carries outside a browser.
//
// The raw value is not a field. It exists once, in the return of
// IssueAPIToken, and is never readable again — the same rule invitations
// follow, and for the same reason.
type APIToken struct {
	ID      uuid.UUID
	UserID  uuid.UUID
	OwnerID string
	Name    string

	// Role the holder has in OwnerID. Carried for the same reason Session
	// carries it: resolving the token already proved the membership exists, so
	// a route gate that re-queried it would be asking a question it has the
	// answer to.
	Role Role

	// UserEmail and TeamName describe who and what the token acts as, so
	// `oz auth whoami` can answer without a second call.
	UserEmail string
	UserName  string
	TeamName  string
	TeamEmail string

	// LastUsedAt is nil for a token that has never been presented. Nil rather
	// than the zero time, because "never used" is what makes a credential safe
	// to delete and the epoch is not a claim anybody would make.
	LastUsedAt *time.Time

	// ExpiresAt is nil for a token that does not expire.
	ExpiresAt *time.Time

	CreatedAt time.Time
}

// Expired reports whether this token has passed its expiry.
//
// For display only. The authority on whether a token is usable is the query,
// which filters expiry in SQL so that a caller which forgets to check here
// cannot treat an expired row as valid.
func (t APIToken) Expired() bool {
	return t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt)
}

// IssueAPIToken mints a credential for a person acting in a team, and returns
// the raw token once.
//
// Membership of teamID is checked here rather than trusted from the caller,
// exactly as CreateSession checks it. The team is what every downstream query
// is scoped by, so a token minted onto a team the person does not belong to is
// a silent grant of that team's apps — and unlike a session, a token has no
// browser to expire out from under it.
//
// A zero ttl means the token does not expire. That is the right default for a
// credential living in a CI secret: an expiry nobody wrote down is a pipeline
// that breaks on a date nobody remembers, and the answer to a leaked token is
// revocation, which is immediate and does not depend on having guessed a
// lifetime correctly.
func (s *Service) IssueAPIToken(
	ctx context.Context, userID uuid.UUID, teamID, name string, ttl time.Duration,
) (string, APIToken, error) {
	if userID == uuid.Nil {
		return "", APIToken{}, errors.New("account: a token needs a user")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", APIToken{}, errors.New("account: a token needs a name")
	}

	role, err := s.RoleIn(ctx, userID, teamID)
	if err != nil {
		return "", APIToken{}, err
	}

	// NewToken's hash is deliberately discarded: it is the hash of the bare
	// value, and what the holder sends is the prefixed one. Storing the bare
	// hash and comparing against the prefixed token would fail every time. The
	// entropy is what is being reused here, not the digest.
	raw, _, err := NewToken()
	if err != nil {
		return "", APIToken{}, err
	}
	raw = TokenPrefix + raw
	hash := HashToken(raw)

	var expires pgtype.Timestamptz
	if ttl > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true}
	}

	row, err := s.q.CreateAPIToken(ctx, dbgen.CreateAPITokenParams{
		UserID:    userID,
		OwnerID:   teamID,
		Name:      name,
		TokenHash: hash,
		ExpiresAt: expires,
	})
	if err != nil {
		// The unique index on (user, team, lower(name)) is what enforces this;
		// checking it here rather than pre-reading avoids a race in which two
		// requests both find the name free.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return "", APIToken{}, ErrTokenNameTaken
		}
		return "", APIToken{}, fmt.Errorf("account: create api token: %w", err)
	}

	tok := tokenFrom(row)
	tok.Role = role
	return raw, tok, nil
}

// uniqueViolation is Postgres's SQLSTATE for a violated unique constraint.
const uniqueViolation = "23505"

// ResolveAPIToken returns the token a raw credential stands for.
//
// The presented value is hashed and looked up by hash; raw values are never
// compared, because there is no raw value stored to compare against.
//
// The returned token carries the role, which the query proved in the same
// statement that proved the credential is live. Nothing here re-reads it: two
// lookups are two answers that can disagree, and the window between them is
// the one in which a demoted member still writes.
func (s *Service) ResolveAPIToken(ctx context.Context, raw string) (APIToken, error) {
	if raw == "" || !strings.HasPrefix(raw, TokenPrefix) {
		return APIToken{}, ErrTokenNotValid
	}

	row, err := s.q.GetAPITokenByHash(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIToken{}, ErrTokenNotValid
		}
		return APIToken{}, fmt.Errorf("account: resolve api token: %w", err)
	}

	// Best effort, and deliberately not part of the resolve. A failure to
	// record that a credential was used must not stop it being used: the write
	// can contend or the pool can be exhausted, and a token that stopped
	// working because its bookkeeping row was busy would be an outage caused
	// entirely by a display field.
	if err := s.q.TouchAPIToken(ctx, row.ID); err != nil {
		s.log.Warn("record api token use", "token", row.ID, "error", err)
	}

	return APIToken{
		ID:         row.ID,
		UserID:     row.UserID,
		OwnerID:    row.OwnerID,
		Name:       row.Name,
		Role:       Role(row.MemberRole),
		UserEmail:  row.UserEmail,
		UserName:   row.UserName,
		TeamName:   row.TeamName,
		TeamEmail:  row.TeamEmail,
		LastUsedAt: timePtr(row.LastUsedAt),
		ExpiresAt:  timePtr(row.ExpiresAt),
		CreatedAt:  row.CreatedAt,
	}, nil
}

// ListAPITokens returns the credentials one person holds in one team.
//
// Scoped by both, so the list is never somebody else's to read. Hashes are not
// returned: the struct has no field for one.
func (s *Service) ListAPITokens(
	ctx context.Context, userID uuid.UUID, teamID string,
) ([]APIToken, error) {
	rows, err := s.q.ListAPITokens(ctx, dbgen.ListAPITokensParams{
		UserID: userID, OwnerID: teamID,
	})
	if err != nil {
		return nil, fmt.Errorf("account: list api tokens: %w", err)
	}
	out := make([]APIToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, tokenFrom(row))
	}
	return out, nil
}

// RevokeAPIToken deletes one.
//
// Scoped by user and team as well as id, so holding an id from somewhere else
// is not enough to revoke somebody's credential. Deleting rather than marking
// revoked: there is nothing about a dead credential worth keeping, and a
// revoked row that still resolves is one forgotten WHERE clause away.
func (s *Service) RevokeAPIToken(
	ctx context.Context, userID uuid.UUID, teamID string, id uuid.UUID,
) error {
	if err := s.q.DeleteAPIToken(ctx, dbgen.DeleteAPITokenParams{
		ID: id, UserID: userID, OwnerID: teamID,
	}); err != nil {
		return fmt.Errorf("account: revoke api token: %w", err)
	}
	return nil
}

func tokenFrom(row dbgen.ApiToken) APIToken {
	return APIToken{
		ID:         row.ID,
		UserID:     row.UserID,
		OwnerID:    row.OwnerID,
		Name:       row.Name,
		LastUsedAt: timePtr(row.LastUsedAt),
		ExpiresAt:  timePtr(row.ExpiresAt),
		CreatedAt:  row.CreatedAt,
	}
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// TokenProvider returns the identity.Provider backed by these API tokens.
func (s *Service) TokenProvider() *APITokens { return &APITokens{svc: s} }

// APITokens resolves the owner of a request from a bearer token.
//
// The fourth identity.Provider the engine ships, alongside SingleOwner,
// StaticToken and Sessions. It exists as a Provider rather than as a special
// case inside the API for the reason the seam exists at all: handlers, the app
// service, the store and the orchestrator already take an OwnerID and never ask
// where it came from, so adding a way to authenticate changes none of them.
//
// It is chained ahead of Sessions rather than replacing them — see
// identity.First. A browser presents a cookie and a CLI presents a header, and
// which one arrived is not something any handler downstream should have to
// know.
type APITokens struct {
	svc *Service
}

var _ identity.Provider = (*APITokens)(nil)

// Resolve returns the token's TEAM as the owner, not its user.
//
// Same rule Sessions.Resolve follows, and it matters more here: a token is
// minted against one team and cannot switch, so the team it resolves to is
// fixed at issue time. Resolving to the person would widen every query to every
// team they belong to, with no session to bound it and no browser to close.
func (p *APITokens) Resolve(ctx context.Context, r *http.Request) (identity.Owner, error) {
	raw := BearerToken(r)
	if raw == "" {
		return identity.Owner{}, identity.ErrUnauthenticated
	}

	tok, err := p.svc.ResolveAPIToken(ctx, raw)
	if err != nil {
		return identity.Owner{}, identity.ErrUnauthenticated
	}

	return identity.Owner{
		ID:          tok.OwnerID,
		DisplayName: tok.TeamName,
		Email:       tok.TeamEmail,
	}, nil
}

// BearerToken returns the credential in an Authorization header, if there is
// one.
//
// Exported because the API's role gate needs the same value the provider read,
// and re-parsing the header in two places is two chances to disagree about what
// counts as a bearer token.
func BearerToken(r *http.Request) string {
	scheme, value, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}
