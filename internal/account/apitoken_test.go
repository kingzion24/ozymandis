package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/identity"
)

func TestAPITokenRoundTrip(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "token", "T")
	if _, err := s.CreateTeam(ctx, "team-token", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	raw, tok, err := s.IssueAPIToken(ctx, u.ID, "team-token", "ci", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	if !strings.HasPrefix(raw, TokenPrefix) {
		t.Errorf("raw token %q does not carry the %q prefix that makes a leak scannable",
			raw, TokenPrefix)
	}
	if tok.Role != RoleOwner {
		t.Errorf("role = %q, want owner — the creator of a team owns it", tok.Role)
	}

	got, err := s.ResolveAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveAPIToken: %v", err)
	}
	if got.UserID != u.ID || got.OwnerID != "team-token" {
		t.Fatalf("token = %+v, want user %s in team-token", got, u.ID)
	}
	if got.Role != RoleOwner {
		t.Errorf("resolved role = %q, want owner", got.Role)
	}
}

// The database must not hold anything that can be replayed.
func TestAPITokenIsStoredAsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "tokenhash", "H")
	_, _ = s.CreateTeam(ctx, "team-tokenhash", "Team", u.ID)
	raw, _, err := s.IssueAPIToken(ctx, u.ID, "team-tokenhash", "ci", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_tokens WHERE encode(token_hash, 'escape') = $1`,
		raw).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("the raw token is recoverable from the database — a dump would be " +
			"a set of working credentials")
	}
}

// The membership join is the security boundary. A token must stop working the
// moment its holder leaves the team, by whatever route they left — including a
// cascade nobody wrote a revocation call for.
func TestAPITokenDiesWithItsMembership(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner := mustUser(t, s, "keeps", "K")
	leaver := mustUser(t, s, "leaves", "L")
	if _, err := s.CreateTeam(ctx, "team-leaver", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-leaver", leaver.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	raw, _, err := s.IssueAPIToken(ctx, leaver.ID, "team-leaver", "laptop", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, raw); err != nil {
		t.Fatalf("token should work while the membership stands: %v", err)
	}

	if err := s.RemoveMember(ctx, owner.ID, "team-leaver", leaver.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if _, err := s.ResolveAPIToken(ctx, raw); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("removed member's token still resolves (err = %v) — revocation "+
			"that depends on remembering to call it is revocation that gets missed", err)
	}
}

// Removal must REVOKE, not suspend.
//
// The distinction is invisible until somebody is re-added. Reads join
// memberships, so a departed member's token stops resolving either way and a
// test that only checked that would pass with the row still sitting in the
// table — which is what it did before the schema grew the composite foreign
// key onto memberships. Removing somebody is what an operator does when a
// laptop is stolen; "and it all works again if they ever rejoin" is not what
// they believe they are doing.
func TestRemovingAMemberDeletesTheirTokens(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner := mustUser(t, s, "readdowner", "O")
	member := mustUser(t, s, "readdmember", "M")
	_, _ = s.CreateTeam(ctx, "team-readd", "Team", owner.ID)
	if err := s.SetRole(ctx, owner.ID, "team-readd", member.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	raw, _, err := s.IssueAPIToken(ctx, member.ID, "team-readd", "laptop", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	if err := s.RemoveMember(ctx, owner.ID, "team-readd", member.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	// The row is gone, not orphaned. Asserted against the table rather than
	// through Resolve, because Resolve cannot tell the two apart.
	var rows int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_tokens WHERE user_id = $1 AND owner_id = $2`,
		member.ID, "team-readd").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d token row(s) survived the removal — the credential is "+
			"suspended, not revoked", rows)
	}

	// And the property that failure would actually cost: re-adding them must
	// not bring the old credential back.
	if err := s.SetRole(ctx, owner.ID, "team-readd", member.ID, RoleAdmin); err != nil {
		t.Fatalf("re-add SetRole: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, raw); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("a token issued before removal works again after re-add "+
			"(err = %v) — removal has to revoke, not suspend", err)
	}
}

// Deleting a team takes its tokens with it, by the same cascade.
func TestDeletingATeamDeletesItsTokens(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "teamgone", "T")
	_, _ = s.CreateTeam(ctx, "team-gone", "Team", u.ID)
	raw, _, err := s.IssueAPIToken(ctx, u.ID, "team-gone", "ci", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	if _, err := s.pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, "team-gone"); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, raw); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("token outlived its team: %v", err)
	}
}

// A role change has to be visible to the next request, not to the next token.
func TestAPITokenCarriesTheCurrentRole(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner := mustUser(t, s, "demoter", "D")
	member := mustUser(t, s, "demoted", "M")
	_, _ = s.CreateTeam(ctx, "team-demote", "Team", owner.ID)
	if err := s.SetRole(ctx, owner.ID, "team-demote", member.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	raw, _, err := s.IssueAPIToken(ctx, member.ID, "team-demote", "ci", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	if got, _ := s.ResolveAPIToken(ctx, raw); got.Role != RoleAdmin {
		t.Fatalf("role = %q, want admin", got.Role)
	}

	if err := s.SetRole(ctx, owner.ID, "team-demote", member.ID, RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	got, err := s.ResolveAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveAPIToken after demotion: %v", err)
	}
	if got.Role != RoleMember {
		t.Errorf("role = %q, want member — a token that reports the role it was "+
			"minted with is a demotion that never takes effect", got.Role)
	}
}

func TestAPITokenExpires(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "expiring", "E")
	_, _ = s.CreateTeam(ctx, "team-expiring", "Team", u.ID)

	raw, _, err := s.IssueAPIToken(ctx, u.ID, "team-expiring", "short", -time.Second)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	// A negative ttl is not an expiry — zero and below both mean "no expiry",
	// so this token must work. The alternative reading, that a caller passing a
	// computed-negative duration gets a dead credential, fails silently at the
	// first use rather than at the mint.
	if _, err := s.ResolveAPIToken(ctx, raw); err != nil {
		t.Fatalf("a non-positive ttl means no expiry: %v", err)
	}

	raw2, _, err := s.IssueAPIToken(ctx, u.ID, "team-expiring", "brief", time.Millisecond)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := s.ResolveAPIToken(ctx, raw2); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("expired token still resolves (err = %v) — expiry is filtered in "+
			"SQL precisely so no caller can forget to check it", err)
	}
}

func TestAPITokenNameIsUniquePerUserAndTeam(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "dupe", "D")
	_, _ = s.CreateTeam(ctx, "team-dupe", "Team", u.ID)

	if _, _, err := s.IssueAPIToken(ctx, u.ID, "team-dupe", "ci", 0); err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}
	// Case-insensitively: "CI" and "ci" read identically in a list, and
	// revoking the wrong one breaks the pipeline while leaving the leak live.
	if _, _, err := s.IssueAPIToken(ctx, u.ID, "team-dupe", "CI", 0); !errors.Is(err, ErrTokenNameTaken) {
		t.Errorf("second token named CI: err = %v, want ErrTokenNameTaken", err)
	}
}

func TestIssueAPITokenRefusesANonMember(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner := mustUser(t, s, "insider", "I")
	outsider := mustUser(t, s, "outsider", "O")
	_, _ = s.CreateTeam(ctx, "team-closed", "Team", owner.ID)

	if _, _, err := s.IssueAPIToken(ctx, outsider.ID, "team-closed", "sneaky", 0); err == nil {
		t.Error("a non-member minted a token onto a team — membership is checked " +
			"here rather than trusted from the caller for exactly this reason")
	}
}

func TestRevokeAPITokenIsScopedToItsHolder(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	a := mustUser(t, s, "holder", "A")
	b := mustUser(t, s, "thief", "B")
	_, _ = s.CreateTeam(ctx, "team-revoke", "Team", a.ID)
	if err := s.SetRole(ctx, a.ID, "team-revoke", b.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	raw, tok, err := s.IssueAPIToken(ctx, a.ID, "team-revoke", "mine", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	// B is in the same team and holds A's token id. That must not be enough.
	if err := s.RevokeAPIToken(ctx, b.ID, "team-revoke", tok.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, raw); err != nil {
		t.Errorf("a teammate revoked somebody else's credential by id: %v", err)
	}

	if err := s.RevokeAPIToken(ctx, a.ID, "team-revoke", tok.ID); err != nil {
		t.Fatalf("RevokeAPIToken by the holder: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, raw); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("revoked token still resolves: %v", err)
	}
}

func TestListAPITokensNeverReturnsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "lister", "L")
	_, _ = s.CreateTeam(ctx, "team-list", "Team", u.ID)
	if _, _, err := s.IssueAPIToken(ctx, u.ID, "team-list", "one", 0); err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	got, err := s.ListAPITokens(ctx, u.ID, "team-list")
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("tokens = %+v, want one named \"one\"", got)
	}
	if got[0].LastUsedAt != nil {
		t.Error("a token that has never been presented must report no last use, " +
			"which is what makes an unused credential safe to prune")
	}
}

// The provider resolves to the TEAM, never the person.
func TestAPITokensProviderResolvesToTheTeam(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "provider", "P")
	_, _ = s.CreateTeam(ctx, "team-provider", "Team", u.ID)
	raw, _, err := s.IssueAPIToken(ctx, u.ID, "team-provider", "cli", 0)
	if err != nil {
		t.Fatalf("IssueAPIToken: %v", err)
	}

	p := s.TokenProvider()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	r.Header.Set("Authorization", "Bearer "+raw)

	owner, err := p.Resolve(ctx, r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if owner.ID != "team-provider" {
		t.Errorf("owner = %q, want team-provider — resolving to the person would "+
			"widen every query to every team they belong to", owner.ID)
	}
	if owner.ID == u.ID.String() {
		t.Error("the provider resolved to the user")
	}
}

func TestAPITokensProviderRejectsWhatIsNotAToken(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	p := s.TokenProvider()
	for _, header := range []string{
		"",
		"Bearer ",
		"Bearer not-a-token",
		"Bearer " + TokenPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"Basic " + TokenPrefix + "whatever",
		TokenPrefix + "whatever",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, err := p.Resolve(ctx, r); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("Authorization %q: err = %v, want ErrUnauthenticated", header, err)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer oz_abc":  "oz_abc",
		"bearer oz_abc":  "oz_abc",
		"BEARER oz_abc":  "oz_abc",
		"Bearer  oz_abc": "oz_abc",
		"Basic oz_abc":   "",
		"oz_abc":         "",
		"":               "",
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestResolveAPITokenRejectsAnUnprefixedValue(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	// A session token is a valid credential of a different kind. Presenting one
	// as a bearer token must not resolve: the prefix check is what stops the
	// two credential spaces from overlapping.
	u := mustUser(t, s, "crossed", "C")
	_, _ = s.CreateTeam(ctx, "team-crossed", "Team", u.ID)
	sess, err := s.CreateSession(ctx, u.ID, "team-crossed", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.ResolveAPIToken(ctx, sess); !errors.Is(err, ErrTokenNotValid) {
		t.Errorf("a session token resolved as an API token: %v", err)
	}
}

func TestIssueAPITokenRefusesABlankName(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "unnamed", "U")
	_, _ = s.CreateTeam(ctx, "team-unnamed", "Team", u.ID)

	for _, name := range []string{"", "   ", "\t"} {
		if _, _, err := s.IssueAPIToken(ctx, u.ID, "team-unnamed", name, 0); err == nil {
			t.Errorf("name %q was accepted — a list of credentials called \"\" is "+
				"a list nobody will ever prune", name)
		}
	}
}

func TestIssueAPITokenRefusesTheNilUser(t *testing.T) {
	s := testService(t)
	if _, _, err := s.IssueAPIToken(context.Background(), uuid.Nil, "team-x", "n", 0); err == nil {
		t.Error("the nil user was accepted")
	}
}
