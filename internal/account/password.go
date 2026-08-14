package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// ErrBadCredentials is returned whenever sign-in fails, whatever the reason.
//
// One error for "no such user", "wrong password" and "this account has no
// password set", because the difference is information about what would have
// worked. A form that says the name was right and the password was not is a
// form that confirms which names exist.
var ErrBadCredentials = errors.New("account: incorrect username or password")

// ErrNotSuperuser means an ordinary user attempted user management.
var ErrNotSuperuser = errors.New("account: only a superuser may manage users")

// ErrUsernameTaken means the name is already in use.
var ErrUsernameTaken = errors.New("account: that username is already taken")

// ErrRejected marks a value the caller can fix by typing a different one.
//
// Wrapped rather than returned, so the specific message still reaches the
// person — "at least 10 characters" is useful, and a handler that mapped every
// rejection to one generic sentence would throw that away. What the sentinel
// buys is the status code: this is a 422, not a 500.
var ErrRejected = errors.New("account: rejected")

// MinPasswordLength is the shortest password that will be accepted.
//
// Length is the only rule. Composition requirements — a digit, a symbol, mixed
// case — measurably push people towards a short password with a predictable
// shape and a "1!" on the end, and this install's whole user base is a handful
// of people the superuser creates by hand.
const MinPasswordLength = 10

// maxPasswordLength is bcrypt's own limit, stated rather than discovered.
//
// bcrypt silently truncates at 72 bytes: a longer password would authenticate
// against its own first 72 bytes, so two different long passwords sharing a
// prefix would both work. Refusing is the honest answer.
const maxPasswordLength = 72

// Authenticate resolves a username and password to the person behind them.
//
// The bcrypt comparison runs even when no such user exists, against a hash
// generated for nobody. Returning early on an unknown name would make sign-in
// measurably faster for names that do not exist, which turns the form into a
// way of enumerating who has an account.
func (s *Service) Authenticate(ctx context.Context, username, password string) (User, error) {
	username = normaliseUsername(username)

	row, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		bcrypt.CompareHashAndPassword(decoyHash, []byte(password))
		return User{}, ErrBadCredentials
	}
	if len(row.PasswordHash) == 0 {
		bcrypt.CompareHashAndPassword(decoyHash, []byte(password))
		return User{}, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)); err != nil {
		return User{}, ErrBadCredentials
	}
	return toUser(row), nil
}

// decoyHash is a valid bcrypt hash of a password nobody holds.
//
// Generated once at init at the same cost as a real one, so that comparing
// against it takes the same time as comparing against a real user's.
var decoyHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("ozymandis-decoy"), bcrypt.DefaultCost)
	if err != nil {
		panic("account: cannot generate decoy hash: " + err.Error())
	}
	decoyHash = h
}

// CreateUser adds a person who can sign in, and is superuser-only.
//
// The actor is passed rather than assumed, and their superuser bit is read from
// the database rather than taken from the caller's copy: a session that was
// minted while somebody was a superuser must not keep creating accounts after
// that has been taken away.
func (s *Service) CreateUser(
	ctx context.Context, actor uuid.UUID, username, password, displayName string, superuser bool,
) (User, error) {
	if err := s.requireSuperuser(ctx, actor); err != nil {
		return User{}, err
	}
	return s.createUser(ctx, username, password, displayName, superuser)
}

func (s *Service) createUser(
	ctx context.Context, username, password, displayName string, superuser bool,
) (User, error) {
	username = normaliseUsername(username)
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}

	row, err := s.q.CreateUser(ctx, dbgen.CreateUserParams{
		Username:     username,
		PasswordHash: hash,
		DisplayName:  strings.TrimSpace(displayName),
		IsSuperuser:  superuser,
	})
	if err != nil {
		// The unique index is what actually decides this, not a prior SELECT:
		// two requests creating the same name concurrently both pass a check
		// and only one passes the constraint.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("account: create user: %w", err)
	}
	return toUser(row), nil
}

// EnsureSuperuser seeds the built-in administrator, and is called at startup.
//
// Idempotent, and deliberately does not rewrite the password of a superuser
// that already exists. An install whose administrator has changed their
// password must not have the built-in default put back over it on the next
// restart — that would make the published default a permanent way in, which is
// the entire problem it is meant to avoid.
func (s *Service) EnsureSuperuser(ctx context.Context, username, password string) (User, error) {
	username = normaliseUsername(username)
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	row, err := s.q.EnsureSuperuser(ctx, dbgen.EnsureSuperuserParams{
		Username:     username,
		PasswordHash: hash,
		DisplayName:  "Superuser",
	})
	if err != nil {
		return User{}, fmt.Errorf("account: ensure superuser: %w", err)
	}
	return toUser(row), nil
}

// ListUsers returns every person on the install, superusers first.
func (s *Service) ListUsers(ctx context.Context, actor uuid.UUID) ([]User, error) {
	if err := s.requireSuperuser(ctx, actor); err != nil {
		return nil, err
	}
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("account: list users: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		out = append(out, toUser(row))
	}
	return out, nil
}

// SetPassword changes somebody's password.
//
// A person may always change their own; changing anybody else's requires being
// a superuser. Sessions are not revoked here — that is the caller's decision,
// because a person changing their own password from one browser should not be
// signed out of it by doing so.
func (s *Service) SetPassword(ctx context.Context, actor, target uuid.UUID, password string) error {
	if actor != target {
		if err := s.requireSuperuser(ctx, actor); err != nil {
			return err
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.q.SetUserPassword(ctx, dbgen.SetUserPasswordParams{
		ID: target, PasswordHash: hash,
	}); err != nil {
		return fmt.Errorf("account: set password: %w", err)
	}
	return nil
}

// DeleteUser removes a person and, by cascade, their sessions and memberships.
//
// A superuser cannot be deleted through here at all — the query itself refuses
// it. The install must always have an administrator, and "delete the last
// superuser" is a state no dialog should be able to reach.
func (s *Service) DeleteUser(ctx context.Context, actor, target uuid.UUID) error {
	if err := s.requireSuperuser(ctx, actor); err != nil {
		return err
	}
	if actor == target {
		return fmt.Errorf("%w: a superuser cannot delete themselves", ErrRejected)
	}
	n, err := s.q.DeleteUser(ctx, target)
	if err != nil {
		return fmt.Errorf("account: delete user: %w", err)
	}
	if n == 0 {
		// One message for both, because the query does not distinguish them and
		// neither outcome is something the caller can act on differently.
		return fmt.Errorf("%w: no such user, or the user is a superuser", ErrRejected)
	}
	return nil
}

// IsSuperuser reports whether the person is an administrator of the install.
func (s *Service) IsSuperuser(ctx context.Context, id uuid.UUID) (bool, error) {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("account: read user: %w", err)
	}
	return row.IsSuperuser, nil
}

func (s *Service) requireSuperuser(ctx context.Context, actor uuid.UUID) error {
	ok, err := s.IsSuperuser(ctx, actor)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotSuperuser
	}
	return nil
}

// HashPassword validates a password and returns its bcrypt hash.
func HashPassword(password string) ([]byte, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("account: hash password: %w", err)
	}
	return hash, nil
}

// ValidatePassword reports why a password is unacceptable, if it is.
func ValidatePassword(password string) error {
	switch {
	case strings.TrimSpace(password) == "":
		return fmt.Errorf("%w: a password is required", ErrRejected)
	case len(password) < MinPasswordLength:
		return fmt.Errorf("%w: a password must be at least %d characters",
			ErrRejected, MinPasswordLength)
	case len(password) > maxPasswordLength:
		return fmt.Errorf(
			"%w: a password must be at most %d bytes — bcrypt ignores anything beyond that",
			ErrRejected, maxPasswordLength)
	}
	return nil
}

// ValidateUsername reports why a username is unacceptable, if it is.
//
// Letters, digits, hyphen and underscore. Narrow on purpose: the name is shown
// in pages, written to logs, and compared case-insensitively, and every
// character class left out is one that cannot later turn a name into something
// that reads as markup or as a second field.
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("%w: a username is required", ErrRejected)
	}
	if len(username) > 39 {
		return fmt.Errorf("%w: a username must be at most 39 characters", ErrRejected)
	}
	for _, r := range username {
		switch {
		case unicode.IsLetter(r) && r < unicode.MaxASCII:
		case unicode.IsDigit(r):
		case r == '-' || r == '_':
		default:
			return fmt.Errorf(
				"%w: a username may contain only letters, digits, hyphen and underscore",
				ErrRejected)
		}
	}
	return nil
}

// normaliseUsername lowercases and trims, matching the unique index.
func normaliseUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
