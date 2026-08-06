-- Where an owner's backups go, and what gets backed up.
--
-- What is absent: any record of the snapshots themselves. Those live in the
-- restic repository, which is the only thing that knows what it actually holds
-- — the same split the rest of this engine keeps between the database, which
-- says what should exist, and the runtime, which says how it is doing. A table
-- of snapshot rows would be a cache that disagrees with the repository the
-- first time a job is killed between writing a snapshot and reporting it, and
-- it would disagree in the direction that matters: claiming a backup exists.

-- name: UpsertBackupDestination :one
INSERT INTO backup_destinations (
    owner_id, endpoint, bucket, prefix, region,
    access_key_id, secret_access_key, repo_password
)
VALUES (@owner_id, @endpoint, @bucket, @prefix, @region,
        @access_key_id, @secret_access_key, @repo_password)
ON CONFLICT (owner_id) DO UPDATE SET
    endpoint          = @endpoint,
    bucket            = @bucket,
    prefix            = @prefix,
    region            = @region,
    access_key_id     = @access_key_id,
    secret_access_key = @secret_access_key,
    repo_password     = @repo_password,
    updated_at        = now()
RETURNING *;

-- name: GetBackupDestination :one
SELECT * FROM backup_destinations WHERE owner_id = @owner_id;

-- Removes the destination.
--
-- Policies are left in place on purpose. They describe what an owner wants
-- backed up, which does not stop being true because the storage was
-- reconfigured; deleting them would mean re-entering every schedule after
-- rotating a bucket credential.
-- name: DeleteBackupDestination :exec
DELETE FROM backup_destinations WHERE owner_id = @owner_id;

-- name: UpsertBackupPolicy :one
INSERT INTO backup_policies (
    owner_id, app_id, enabled, schedule, keep_daily, keep_weekly, keep_monthly
)
VALUES (@owner_id, @app_id, @enabled, @schedule,
        @keep_daily, @keep_weekly, @keep_monthly)
ON CONFLICT (app_id) DO UPDATE SET
    enabled      = @enabled,
    schedule     = @schedule,
    keep_daily   = @keep_daily,
    keep_weekly  = @keep_weekly,
    keep_monthly = @keep_monthly,
    updated_at   = now()
RETURNING *;

-- name: GetBackupPolicy :one
SELECT * FROM backup_policies WHERE app_id = @app_id;

-- name: DeleteBackupPolicy :execrows
DELETE FROM backup_policies WHERE owner_id = @owner_id AND app_id = @app_id;

-- Every policy an owner has, with the app it belongs to.
--
-- Joined rather than returning app ids for the caller to look up one at a
-- time: the settings page lists these together, and the n+1 is the whole of
-- what that page does.
-- name: ListBackupPolicies :many
SELECT sqlc.embed(p), a.name AS app_name, a.namespace AS app_namespace,
       a.source AS app_source
FROM backup_policies p
JOIN apps a ON a.id = p.app_id
WHERE p.owner_id = @owner_id
ORDER BY a.name;

-- Policies that are switched on, for reconciling schedules into the cluster
-- at startup. A disabled one is reconciled too — as a schedule to remove —
-- which is why this does not filter on enabled.
-- name: ListBackupPoliciesForReconcile :many
SELECT sqlc.embed(p), a.name AS app_name, a.namespace AS app_namespace,
       a.source AS app_source
FROM backup_policies p
JOIN apps a ON a.id = p.app_id
ORDER BY p.owner_id, a.name;
