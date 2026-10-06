-- Builds. Owner-scoped in the row rather than through the deployment, because
-- the log is readable output and the scoping has to be checkable in the query
-- that reads it.

-- name: CreateBuild :one
INSERT INTO builds (owner_id, app_id, deployment_id, repo_url, repo_ref)
VALUES (@owner_id, @app_id, @deployment_id, @repo_url, @repo_ref)
RETURNING *;

-- name: AppendBuildLog :exec
-- Appended rather than replaced, so a build streaming its output does not have
-- to hold the whole log in memory to write any of it.
UPDATE builds
SET log = log || @chunk::text
WHERE id = @id;

-- name: FinishBuild :one
-- Only a build still running can be finished. Two things write this — the
-- goroutine that ran the build and the reconciler settling one whose process
-- went away — and without the guard the second overwrote the first: a build
-- the reconciler had failed, along with its deployment, was rewritten to
-- succeeded a moment later and left a succeeded build under a failed deploy.
-- Whoever arrives second gets no row and knows it lost.
UPDATE builds
SET status      = @status,
    message     = @message,
    image       = @image,
    commit_sha  = @commit_sha,
    finished_at = now()
WHERE id = @id AND status = 'running'
RETURNING *;

-- name: GetBuildForDeployment :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND deployment_id = @deployment_id;

-- name: GetBuild :one
SELECT * FROM builds
WHERE owner_id = @owner_id AND id = @id;

-- name: SetBuildJob :exec
UPDATE builds SET job_name = @job_name WHERE id = @id;

-- name: ListRunningBuilds :many
-- Every build still claiming to run, oldest first. Not owner-scoped: this
-- feeds the reconciler, which is the platform settling its own records rather
-- than a team reading theirs.
SELECT * FROM builds WHERE status = 'running' ORDER BY started_at;
