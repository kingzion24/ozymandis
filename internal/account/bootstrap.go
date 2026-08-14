package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// BootstrapOwner gives someone with no team one to act as, handing the first of
// them the team the install was already running as.
//
// This is the spec's third lockout guard. An install that switches accounts on
// has apps already deployed under OZYMANDIS_OWNER_ID; without this the first person
// to sign in lands in a fresh empty team and those apps belong to an owner
// nobody can authenticate as — visible only as a dashboard that has lost
// everything.
//
// The configured team is claimed only when it has no owner at all, and that is
// read and acted on inside one transaction holding the team row. Two people
// completing sign-in at the same moment therefore produce exactly one owner:
// the loser takes the second branch and gets a team of their own. The check is
// "has no owner" rather than "has no members" so that an install whose team was
// left with members but no owner is still recoverable.
//
// Idempotent: someone who already holds a role in the configured team is left
// exactly as they are, so a retried sign-in cannot mint a second team.
func (s *Service) BootstrapOwner(
	ctx context.Context, teamID, teamName string, user User,
) error {
	if user.ID == uuid.Nil {
		return errors.New("account: bootstrapping an owner needs a user")
	}
	teamID = strings.TrimSpace(teamID)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	claimed := teamID != ""
	if claimed {
		// FOR UPDATE is what serialises the count below against a second
		// sign-in. On a first install the row does not exist yet and there is
		// nothing to lock — the insert's own unique-index wait takes that job,
		// which is why the team is created before the count rather than after.
		if _, err := q.LockTeam(ctx, teamID); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("account: lock team: %w", err)
			}
			if _, err := q.CreateTeam(ctx, dbgen.CreateTeamParams{
				ID: teamID, DisplayName: teamName,
			}); err != nil {
				return fmt.Errorf("account: create team: %w", err)
			}
		}

		switch _, err := roleIn(ctx, q, user.ID, teamID); {
		case err == nil:
			// Already a member: there is nothing to hand out, and handing them a
			// second team would be the retry doing damage.
			return tx.Commit(ctx)
		case !errors.Is(err, ErrNotAMember):
			return err
		}

		owners, err := q.CountOwnersOfTeam(ctx, teamID)
		if err != nil {
			return fmt.Errorf("account: count owners: %w", err)
		}
		claimed = owners == 0
	}

	if !claimed {
		// Somebody else got there first, or there was no configured team to get
		// to. They still need somewhere to act, or the session resolves to no
		// owner and the dashboard is a wall.
		teamID, teamName = personalTeam(user)
		if _, err := q.CreateTeam(ctx, dbgen.CreateTeamParams{
			ID: teamID, DisplayName: teamName,
		}); err != nil {
			return fmt.Errorf("account: create team: %w", err)
		}
	}

	if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
		UserID: user.ID, OwnerID: teamID, Role: string(RoleOwner),
	}); err != nil {
		return fmt.Errorf("account: create owner membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}

	s.log.Info("owner bootstrapped",
		slog.String("team", teamID),
		slog.String("user", user.ID.String()),
		slog.Bool("inherited", claimed))
	return nil
}

// personalTeam names the team someone gets when the configured one is taken.
//
// The id is derived from the user id rather than from their address: two people
// at different domains share a local part, and a team id collision would hand
// one of them the other's apps.
func personalTeam(user User) (id, name string) {
	name = strings.TrimSpace(user.DisplayName)
	if name == "" {
		name = user.Username
	}
	return "user-" + user.ID.String(), name
}
