-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, owner_id, name, token_hash, expires_at)
VALUES (@user_id, @owner_id, @name, @token_hash, @expires_at)
RETURNING *;

-- Resolves a token only while the membership behind it still exists, and
-- returns the role in the same row.
--
-- This is GetSessionByHash with a different credential, and it is deliberately
-- the same shape in all three respects that matter:
--
-- The join to memberships is the security boundary, not decoration. A token
-- outlives the browser that minted it by design, so "revoke the credential when
-- someone leaves" is a step that will be missed — here it cannot be, because a
-- departed member's token stops resolving the instant the membership row goes,
-- by whatever route it went, including cascade.
--
-- The row is deleted by that same cascade rather than merely orphaned; the
-- schema carries a composite foreign key onto memberships to ensure it. This
-- join is therefore defence in depth rather than the whole mechanism, which is
-- the right way round: were it the only mechanism, removing somebody would
-- suspend their tokens rather than revoke them, and re-adding them would bring
-- every one of those credentials back.
--
-- The role comes back with the credential because the query that proves the
-- token is live is the query that says what it may do. Looking the role up
-- separately gives two answers that can disagree, and the window between them
-- is the one where a demoted member still writes.
--
-- Expiry is filtered in SQL rather than in Go so that an expired row can never
-- be treated as valid by a caller that forgets to check. NULL means no expiry,
-- which is the common case for a credential living in CI.
-- name: GetAPITokenByHash :one
SELECT t.*, u.email AS user_email, u.display_name AS user_name,
       m.role AS member_role, tm.display_name AS team_name, tm.email AS team_email
FROM api_tokens t
JOIN users u ON u.id = t.user_id
JOIN teams tm ON tm.id = t.owner_id
JOIN memberships m ON m.owner_id = t.owner_id AND m.user_id = t.user_id
WHERE t.token_hash = @token_hash
  AND (t.expires_at IS NULL OR t.expires_at > now());

-- The list a person prunes from. Scoped by both user and team: a token is a
-- credential of one person acting as one team, and listing another person's is
-- neither useful nor theirs to see.
-- name: ListAPITokens :many
SELECT * FROM api_tokens
WHERE user_id = @user_id AND owner_id = @owner_id
ORDER BY created_at DESC;

-- Deleted by id, scoped by user and team so that holding an id from somewhere
-- else is not enough to revoke somebody's credential.
-- name: DeleteAPIToken :exec
DELETE FROM api_tokens
WHERE id = @id AND user_id = @user_id AND owner_id = @owner_id;

-- Best-effort bookkeeping on a hot path. Separate from GetAPITokenByHash rather
-- than folded into it with a CTE: resolving a credential is a read, and making
-- it a write puts every authenticated API request into a row lock on the same
-- token. A CI job running twenty parallel deploys would serialise on it.
-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = @id;

-- name: DeleteExpiredAPITokens :exec
DELETE FROM api_tokens WHERE expires_at IS NOT NULL AND expires_at <= now();
