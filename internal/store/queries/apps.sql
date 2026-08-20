-- Every query filters by owner_id.
--
-- This is not defensive duplication of an application-level check — it is the
-- check. A handler that forgets to scope produces no rows here rather than
-- another owner's data, and sqlc makes the parameter impossible to omit
-- because the generated signature requires it.

-- name: CreateTeamRow :one
INSERT INTO teams (id, display_name, email)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        email        = EXCLUDED.email,
        updated_at   = now()
RETURNING *;

-- name: GetTeamRow :one
SELECT * FROM teams WHERE id = $1;

-- name: CreateApp :one
INSERT INTO apps (
    owner_id, name, namespace, image, replicas, port, source, internal,
    cpu_request, cpu_limit, memory_request, memory_limit, project_id,
    repo_url, repo_branch, repo_subdir, command
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: GetApp :one
SELECT * FROM apps
WHERE owner_id = $1 AND name = $2;

-- name: GetAppByID :one
SELECT * FROM apps
WHERE owner_id = $1 AND id = $2;

-- name: ListApps :many
SELECT * FROM apps
WHERE owner_id = $1
ORDER BY name;

-- name: CountApps :one
SELECT count(*) FROM apps WHERE owner_id = $1;

-- name: UpdateApp :one
UPDATE apps
SET image          = $3,
    replicas       = $4,
    port           = $5,
    cpu_request    = $6,
    cpu_limit      = $7,
    memory_request = $8,
    memory_limit   = $9,
    updated_at     = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: SetAppReplicas :one
UPDATE apps
SET replicas = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: DeleteApp :exec
DELETE FROM apps WHERE owner_id = $1 AND id = $2;

-- name: CreateDeployment :one
INSERT INTO deployments (owner_id, app_id, image, revision, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: SetDeploymentImage :one
-- The image a build produced, recorded on the deployment that produced it.
--
-- CreateDeployment can only store the image the app had when the deploy was
-- asked for, which for a git app is the *previous* deploy's image: the build
-- that makes the new one has not run yet. Left there, every row in the history
-- names the image it replaced, and rolling back to what a row says would
-- redeploy the wrong one.
UPDATE deployments
SET image = $3
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: FinishDeployment :one
UPDATE deployments
SET status = $3, message = $4, finished_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: ListDeployments :many
-- Joined to apps for the source, which the deployment row does not carry.
--
-- Correct for old rows as well as new ones: apps.source is written by
-- CreateApp and by nothing else, so an app's source cannot have been anything
-- different when an earlier deployment ran.
SELECT d.*, a.source AS app_source
FROM deployments d
JOIN apps a ON a.id = d.app_id AND a.owner_id = d.owner_id
WHERE d.owner_id = $1 AND d.app_id = $2
ORDER BY d.started_at DESC
LIMIT $3;

-- name: ListRecentDeployments :many
-- Joined to apps so the activity feed can name the workload without a second
-- round trip per row. The join is on app_id AND owner_id: joining on app_id
-- alone would be correct today and wrong the moment more than one owner exists.
SELECT d.*, a.name AS app_name, a.namespace AS app_namespace, a.source AS app_source
FROM deployments d
JOIN apps a ON a.id = d.app_id AND a.owner_id = d.owner_id
WHERE d.owner_id = $1
ORDER BY d.started_at DESC
LIMIT $2;

-- name: DeployActivity :many
-- Deploys per day and outcome, for the overview chart.
--
-- Counted in the database rather than by reading rows and tallying them in Go:
-- a busy month is thousands of deployments, and none of them are wanted here
-- except as a number.
--
-- Days with no deploys are absent from this result. The caller fills them in;
-- see app.DeployActivity for why that cannot be skipped.
SELECT date_trunc('day', started_at)::timestamptz AS day,
       status,
       count(*)::bigint AS total
FROM deployments
WHERE owner_id = $1 AND started_at >= $2
GROUP BY 1, 2
ORDER BY 1;

-- name: SetAppHealth :one
UPDATE apps
SET health_path     = @health_path,
    health_liveness = @health_liveness,
    updated_at      = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- name: SetAppCommand :one
UPDATE apps
SET command    = @command,
    updated_at = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- Both together, because they are one decision.
--
-- A port is what traffic is routed to and internal is whether any is routed at
-- all; setting one without the other produces states nobody asked for — an
-- internal app that just acquired a port, or a public app whose port was
-- cleared and which therefore has a hostname routing to nothing. The caller
-- passes both and the pair is written atomically.
--
-- Narrow rather than reusing UpdateApp, which also carries image and replicas.
-- Those belong to the deploy and the scale paths respectively, and a query that
-- can write all five is one that will eventually write a stale image while
-- changing a port.
-- name: SetAppService :one
UPDATE apps
SET port       = @port,
    internal   = @internal,
    updated_at = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- name: SetAppNetworking :execrows
UPDATE apps
SET https_only = @https_only, cname_only = @cname_only, updated_at = now()
WHERE owner_id = @owner_id AND name = @name;

-- Retires the deployments a new one replaces.
--
-- Only rows that never reached a terminal state: a finished deployment already
-- says what happened to it, and rewriting that would lose the difference
-- between one that was replaced and one that failed.
-- name: SupersedeDeployments :execrows
UPDATE deployments
SET status = 'superseded', finished_at = COALESCE(finished_at, now())
WHERE owner_id = @owner_id AND app_id = @app_id
  AND status IN ('running', 'active');

-- name: GetDeployment :one
SELECT * FROM deployments
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppImage :one
-- The image a build produced. Separate from UpdateApp because a build sets
-- only this: the replicas and limits a person configured are not a build's to
-- overwrite, and passing them through would make every build a chance to.
UPDATE apps
SET image = $3, updated_at = now()
WHERE owner_id = $1 AND id = $2
RETURNING *;

-- name: SetAppRunAsUser :exec
-- Recorded by a build, which is the only thing that can discover it.
UPDATE apps SET run_as_user = @run_as_user, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppReleaseCommand :one
UPDATE apps
SET release_command = @release_command,
    updated_at      = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- Written whether the release passed or failed.
--
-- A failed release's reason is in its log, and dropping the log on failure is
-- how a vetoed deploy becomes impossible to explain — which is the one case
-- somebody actually needs it.
-- name: SetDeploymentRelease :exec
UPDATE deployments
SET release_status = @release_status,
    release_log    = @release_log
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppAutoDeploy :one
UPDATE apps
SET auto_deploy = @auto_deploy, updated_at = now()
WHERE owner_id = @owner_id AND id = @id
RETURNING *;

-- name: SetAppWebhookSecret :exec
UPDATE apps SET webhook_secret = @webhook_secret, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppDeployKey :exec
UPDATE apps
SET deploy_key = @deploy_key, deploy_key_public = @deploy_key_public, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- name: SetAppLastDeployedSHA :exec
UPDATE apps SET last_deployed_sha = @last_deployed_sha, updated_at = now()
WHERE owner_id = @owner_id AND id = @id;

-- Candidates a push might affect.
--
-- Deliberately NOT filtered by repository URL in SQL. The URL in a webhook
-- payload is attacker-controlled — anybody can POST a body naming any
-- repository — so it must never be the thing that selects which app is
-- deployed. The signature does that: every candidate is tried and only the one
-- whose secret verifies is acted on, which is why this returns them all.
-- name: ListAutoDeployApps :many
SELECT * FROM apps WHERE auto_deploy AND webhook_secret IS NOT NULL;

-- name: ListAutoDeployAppsForOwner :many
SELECT * FROM apps WHERE owner_id = @owner_id AND auto_deploy;
