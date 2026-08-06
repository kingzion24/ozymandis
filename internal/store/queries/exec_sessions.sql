-- Written BEFORE the stream opens, so a session that dies during attach has
-- already been recorded. See 00025 for why the ordering is load-bearing.
-- name: StartExecSession :one
INSERT INTO exec_sessions (owner_id, app_id, actor, pod, command, tty)
VALUES (@owner_id, @app_id, @actor, @pod, @command, @tty)
RETURNING *;

-- Written on a detached, bounded context at teardown.
--
-- Scoped by owner as well as id: an audit row is not something a caller should
-- be able to close on somebody else's behalf by holding an id.
-- name: EndExecSession :exec
UPDATE exec_sessions
SET ended_at  = now(),
    outcome   = @outcome,
    exit_code = @exit_code
WHERE owner_id = @owner_id AND id = @id;

-- name: ListExecSessions :many
SELECT s.*, a.name AS app_name
FROM exec_sessions s
JOIN apps a ON a.id = s.app_id AND a.owner_id = s.owner_id
WHERE s.owner_id = @owner_id
ORDER BY s.started_at DESC
LIMIT @row_limit;

-- The sessions that have not ended.
--
-- The signal this table exists to carry, and it needs a query or it is a signal
-- nobody can read. Two different situations land here and both are worth
-- seeing:
--
--   * somebody is in a container RIGHT NOW, which is what an incident asks
--   * a session ended in a way this process never observed — it was killed
--     with the process, or the machine went away — so nothing ever closed it
--
-- They are deliberately not distinguished, because nothing observed the
-- difference. A row that has been open for four seconds is somebody working; a
-- row open for four days is a session nobody will ever account for, and the
-- started_at is what tells them apart.
--
-- Backed by the partial index on (owner_id) WHERE ended_at IS NULL.
-- name: ListOpenExecSessions :many
SELECT s.*, a.name AS app_name
FROM exec_sessions s
JOIN apps a ON a.id = s.app_id AND a.owner_id = s.owner_id
WHERE s.owner_id = @owner_id AND s.ended_at IS NULL
ORDER BY s.started_at DESC;
