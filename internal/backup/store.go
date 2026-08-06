package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kingzion24/ozymandis/internal/secret"
	"github.com/kingzion24/ozymandis/internal/store/dbgen"
)

// LoadDestination reads and unseals an owner's destination.
//
// Returns ErrNoDestination when there is none, which callers treat as the
// feature being off rather than as a failure.
func LoadDestination(
	ctx context.Context, q *dbgen.Queries, keeper *secret.Keeper, ownerID string,
) (Destination, error) {
	row, err := q.GetBackupDestination(ctx, ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, ErrNoDestination
	}
	if err != nil {
		return Destination{}, fmt.Errorf("backup: read destination: %w", err)
	}
	if keeper == nil {
		return Destination{}, errors.New(
			"backup: no secret key configured, so the stored credentials cannot be opened")
	}

	secretKey, err := keeper.Open(row.SecretAccessKey)
	if err != nil {
		return Destination{}, fmt.Errorf("backup: open secret access key: %w", err)
	}
	repoPassword, err := keeper.Open(row.RepoPassword)
	if err != nil {
		return Destination{}, fmt.Errorf("backup: open repository password: %w", err)
	}

	return Destination{
		Endpoint:        row.Endpoint,
		Bucket:          row.Bucket,
		Prefix:          row.Prefix,
		Region:          row.Region,
		AccessKeyID:     row.AccessKeyID,
		SecretAccessKey: secretKey,
		RepoPassword:    repoPassword,
	}, nil
}

// SaveDestination validates, seals and stores a destination.
func SaveDestination(
	ctx context.Context, q *dbgen.Queries, keeper *secret.Keeper,
	ownerID string, d Destination,
) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if keeper == nil {
		return errors.New("backup: set OZYMANDIS_SECRET_KEY before storing backup " +
			"credentials — they would otherwise be readable in every database dump, " +
			"including the ones this feature makes")
	}

	sealedSecret, err := keeper.Seal(d.SecretAccessKey)
	if err != nil {
		return fmt.Errorf("backup: seal secret access key: %w", err)
	}
	sealedPassword, err := keeper.Seal(d.RepoPassword)
	if err != nil {
		return fmt.Errorf("backup: seal repository password: %w", err)
	}

	region := d.Region
	if region == "" {
		region = "auto"
	}

	if _, err := q.UpsertBackupDestination(ctx, dbgen.UpsertBackupDestinationParams{
		OwnerID:         ownerID,
		Endpoint:        d.Endpoint,
		Bucket:          d.Bucket,
		Prefix:          d.Prefix,
		Region:          region,
		AccessKeyID:     d.AccessKeyID,
		SecretAccessKey: sealedSecret,
		RepoPassword:    sealedPassword,
	}); err != nil {
		return fmt.Errorf("backup: store destination: %w", err)
	}
	return nil
}

// LoadPolicy reads an app's policy, or ErrNoPolicy.
func LoadPolicy(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) (Policy, error) {
	row, err := q.GetBackupPolicy(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNoPolicy
	}
	if err != nil {
		return Policy{}, fmt.Errorf("backup: read policy: %w", err)
	}
	return toPolicy(row), nil
}

// SavePolicy validates and stores an app's policy.
func SavePolicy(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID, p Policy,
) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := q.UpsertBackupPolicy(ctx, dbgen.UpsertBackupPolicyParams{
		OwnerID:     ownerID,
		AppID:       appID,
		Enabled:     p.Enabled,
		Schedule:    p.Schedule,
		KeepDaily:   int32(p.KeepDaily),
		KeepWeekly:  int32(p.KeepWeekly),
		KeepMonthly: int32(p.KeepMonthly),
	}); err != nil {
		return fmt.Errorf("backup: store policy: %w", err)
	}
	return nil
}

// DeletePolicy stops backing an app up. It does not touch what has already been
// written: snapshots outlive the policy that made them, which is the point of
// having them.
func DeletePolicy(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
) error {
	if _, err := q.DeleteBackupPolicy(ctx, dbgen.DeleteBackupPolicyParams{
		OwnerID: ownerID, AppID: appID,
	}); err != nil {
		return fmt.Errorf("backup: delete policy: %w", err)
	}
	return nil
}

func toPolicy(row dbgen.BackupPolicy) Policy {
	return Policy{
		AppID:       row.AppID.String(),
		Enabled:     row.Enabled,
		Schedule:    row.Schedule,
		KeepDaily:   int(row.KeepDaily),
		KeepWeekly:  int(row.KeepWeekly),
		KeepMonthly: int(row.KeepMonthly),
	}
}
