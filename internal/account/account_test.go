package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kingzion24/ozymandis/internal/store"
)

// testService migrates, connects and returns a Service against the test
// database, skipping when none is configured so `go test ./...` stays useful
// on a laptop without Postgres.
//
// Leftovers are purged before the test as well as after it, so a run that
// crashed halfway does not fail the next one on a unique constraint. The pool's
// Cleanup is registered first because cleanups run last-registered-first and
// the purge has to happen while the pool is still open.
func testService(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("OZYMANDIS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OZYMANDIS_TEST_DATABASE_URL to run account tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Memberships, sessions and invitations follow by cascade. The prefixes
	// keep this clear of the owner ids the store, app and domain packages use,
	// because `go test ./...` runs those packages against the same database at
	// the same time.
	//
	// This purge is why every package owns a username prefix. It used to be an
	// email suffix and moved with the identity when passwords replaced links.
	//
	// The DELETE below is unscoped beyond the prefix: it removes every user
	// whose name begins with it, including rows another package inserted a
	// microsecond ago and is about to reference. That surfaces in the other
	// package as a foreign key violation naming a table this one never touched,
	// which is close to unreadable as a failure.
	//
	// So at- belongs to this package. Others use their own — web uses wt-,
	// store uses store-test- — and a new package creating users should pick its
	// own rather than reach for the obvious one. If you arrived here after a
	// flaky FK error in some other package, that is the bug, and the fix is on
	// that package's side.
	//
	// The prefix also survives the process: the suffix counter restarts with
	// each run, so without purging by it the second run collides with the
	// first's rows.
	purge := func() {
		// The team the bootstrap gives someone who could not have the configured
		// one is named after them, so it is found through the same filter — and
		// has to go before the user it is derived from.
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id IN (
			SELECT 'user-' || u.id::text FROM users u
			WHERE u.username LIKE $1)`, testPrefix+"%"); err != nil {
			t.Errorf("purge personal teams: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'team-%'`); err != nil {
			t.Errorf("purge teams: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM users WHERE username LIKE $1`, testPrefix+"%"); err != nil {
			t.Errorf("purge users: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	return NewService(pool, log)
}

func TestRoleOrdering(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleOwner.AtLeast(RoleMember) {
		t.Error("owner must outrank admin and member")
	}
	if !RoleAdmin.AtLeast(RoleMember) {
		t.Error("admin must outrank member")
	}
	if RoleMember.AtLeast(RoleAdmin) {
		t.Error("member must not outrank admin")
	}
	if !RoleAdmin.CanAdminister() || RoleMember.CanAdminister() {
		t.Error("CanAdminister: admin yes, member no")
	}
	if !RoleOwner.CanOwn() || RoleAdmin.CanOwn() {
		t.Error("CanOwn: owner only")
	}
}

// Usernames are one identity regardless of case. Treating Batman and batman as
// two people is a second account nobody can sign in to.
func TestUsernamesAreCaseInsensitive(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	name := uniqueName("alice")
	if _, err := s.createUser(ctx, strings.ToUpper(name), "correct-horse", "Alice", false); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := s.createUser(ctx, name, "another-one-1", "Alice again", false); !errors.Is(
		err, ErrUsernameTaken) {
		t.Fatalf("second createUser err = %v, want ErrUsernameTaken", err)
	}

	// And the same name in any case authenticates the one account.
	for _, tried := range []string{name, strings.ToUpper(name), strings.ToTitle(name)} {
		if _, err := s.Authenticate(ctx, tried, "correct-horse"); err != nil {
			t.Fatalf("Authenticate(%q): %v", tried, err)
		}
	}
}

// Every way of failing must be one error. A caller that could tell "no such
// user" from "wrong password" could enumerate who has an account.
func TestAuthenticateRefusesUniformly(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	name := uniqueName("bruce")
	if _, err := s.createUser(ctx, name, "correct-horse", "", false); err != nil {
		t.Fatalf("createUser: %v", err)
	}

	for _, tc := range []struct{ name, user, pass string }{
		{"wrong password", name, "wrong-horse-1"},
		{"no such user", uniqueName("nobody"), "correct-horse"},
		{"empty password", name, ""},
	} {
		if _, err := s.Authenticate(ctx, tc.user, tc.pass); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s: err = %v, want ErrBadCredentials", tc.name, err)
		}
	}
}

// The seed must never rewrite a password that has been changed, or every
// restart would put the published default back.
func TestEnsureSuperuserDoesNotResetAnExistingPassword(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	boss := uniqueName("batman")
	first, err := s.EnsureSuperuser(ctx, boss, "built-in-default")
	if err != nil {
		t.Fatalf("EnsureSuperuser: %v", err)
	}
	if err := s.SetPassword(ctx, first.ID, first.ID, "chosen-by-a-person"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// A restart.
	if _, err := s.EnsureSuperuser(ctx, boss, "built-in-default"); err != nil {
		t.Fatalf("EnsureSuperuser again: %v", err)
	}

	if _, err := s.Authenticate(ctx, boss, "built-in-default"); !errors.Is(
		err, ErrBadCredentials) {
		t.Fatal("the built-in default still works after the password was changed")
	}
	if _, err := s.Authenticate(ctx, boss, "chosen-by-a-person"); err != nil {
		t.Fatalf("the chosen password stopped working: %v", err)
	}
}

// Only a superuser may manage people, and it is read from the database rather
// than from anything the caller says about itself.
func TestUserManagementIsSuperuserOnly(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	boss, err := s.EnsureSuperuser(ctx, uniqueName("batman"), "built-in-default")
	if err != nil {
		t.Fatalf("EnsureSuperuser: %v", err)
	}
	ordinary, err := s.createUser(ctx, uniqueName("robin"), "sidekick-1234", "", false)
	if err != nil {
		t.Fatalf("createUser: %v", err)
	}

	if _, err := s.CreateUser(ctx, ordinary.ID, uniqueName("joker"), "why-so-serious", "", false); !errors.Is(
		err, ErrNotSuperuser) {
		t.Errorf("CreateUser as an ordinary user: err = %v, want ErrNotSuperuser", err)
	}
	if _, err := s.ListUsers(ctx, ordinary.ID); !errors.Is(err, ErrNotSuperuser) {
		t.Errorf("ListUsers as an ordinary user: err = %v, want ErrNotSuperuser", err)
	}
	if err := s.DeleteUser(ctx, ordinary.ID, boss.ID); !errors.Is(err, ErrNotSuperuser) {
		t.Errorf("DeleteUser as an ordinary user: err = %v, want ErrNotSuperuser", err)
	}

	// And a superuser cannot be deleted even by a superuser.
	if err := s.DeleteUser(ctx, boss.ID, boss.ID); err == nil {
		t.Error("a superuser deleted themselves")
	}
}

func TestCreateTeamMakesTheCreatorAnOwner(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "owner", "Owner")
	if _, err := s.CreateTeam(ctx, "team-create", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	role, err := s.RoleIn(ctx, u.ID, "team-create")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner", role)
	}

	teams, err := s.TeamsFor(ctx, u.ID)
	if err != nil {
		t.Fatalf("TeamsFor: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamID != "team-create" || teams[0].Role != RoleOwner {
		t.Fatalf("memberships = %+v, want one owner membership of team-create", teams)
	}
}

// The id is a slug someone chooses. Creating over a team that already has an
// owner would hand the caller that team's apps.
func TestCreateTeamRefusesAnIdSomeoneElseHolds(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	held := mustUser(t, s, "holder", "Holder")
	if _, err := s.CreateTeam(ctx, "team-taken", "Team", held.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	other := mustUser(t, s, "other", "Other")
	if _, err := s.CreateTeam(ctx, "team-taken", "Mine Now", other.ID); !errors.Is(err, ErrTeamTaken) {
		t.Fatalf("want ErrTeamTaken, got %v", err)
	}
	if _, err := s.RoleIn(ctx, other.ID, "team-taken"); !errors.Is(err, ErrNotAMember) {
		t.Fatal("the refused creation still made them a member")
	}

	// Re-creating a team you already own stays idempotent, so an operator can
	// re-run the bootstrap that made it.
	if _, err := s.CreateTeam(ctx, "team-taken", "Renamed", held.ID); err != nil {
		t.Fatalf("the owner re-creating their own team: %v", err)
	}
}

// A team with no owner cannot be administered by anyone ever again.
func TestLastOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "solo", "Solo")
	if _, err := s.CreateTeam(ctx, "team-last-owner", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	if err := s.SetRole(ctx, u.ID, "team-last-owner", u.ID, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last owner: want ErrLastOwner, got %v", err)
	}
	if err := s.RemoveMember(ctx, u.ID, "team-last-owner", u.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last owner: want ErrLastOwner, got %v", err)
	}

	// The refusals must have changed nothing.
	role, err := s.RoleIn(ctx, u.ID, "team-last-owner")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role after two refused changes = %q, want owner", role)
	}
}

func TestOnlyAnOwnerCanManageAdmins(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner := mustUser(t, s, "o", "O")
	admin := mustUser(t, s, "a", "A")
	if _, err := s.CreateTeam(ctx, "team-roles", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-roles", admin.ID, RoleAdmin); err != nil {
		t.Fatalf("owner promoting to admin: %v", err)
	}

	// An admin must not be able to make another admin — that is ownership.
	member := mustUser(t, s, "m", "M")
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleAdmin); err == nil {
		t.Fatal("an admin promoted someone to admin")
	}
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleMember); err != nil {
		t.Fatalf("an admin must be able to add a member: %v", err)
	}

	// Nor may an admin unseat the person who appointed them.
	if err := s.RemoveMember(ctx, admin.ID, "team-roles", owner.ID); err == nil {
		t.Fatal("an admin removed an owner")
	}
	if err := s.RemoveMember(ctx, member.ID, "team-roles", admin.ID); err == nil {
		t.Fatal("a member removed an admin")
	}
}

func TestRoleInReportsNonMembers(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u := mustUser(t, s, "stranger", "S")
	if _, err := s.RoleIn(ctx, u.ID, "team-does-not-exist"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("want ErrNotAMember, got %v", err)
	}
}

// mustUser creates a person who can sign in, failing the test if it cannot.
//
// Tests care about roles and memberships rather than credentials, so the
// password is fixed and never asserted on — it exists only because an account
// with no password is one that cannot authenticate.
func mustUser(t *testing.T, s *Service, username, displayName string) User {
	t.Helper()

	// Suffixed, because the database outlives any one test in this package and
	// usernames are unique across all of it. Two tests both wanting "holder"
	// would otherwise fail on whichever ran second, which reads as a bug in the
	// code rather than in the fixture.
	name := uniqueName(username)

	u, err := s.createUser(context.Background(), name, "test-password-1", displayName, false)
	if err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
	return u
}

var userSeq atomic.Int64

// testPrefix marks every account this package creates, so the purge can find
// them without knowing what any individual test called them.
const testPrefix = "at-"

// uniqueName suffixes a username so it cannot collide with one another test in
// this package already took. The database outlives any single test, and the
// counter restarts with the process.
func uniqueName(base string) string {
	return fmt.Sprintf("%s%s-%d", testPrefix, base, userSeq.Add(1))
}
