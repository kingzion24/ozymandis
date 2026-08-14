-- Users and sessions carry no owner_id: a person exists before they belong to
-- any team. Memberships do, and stay scoped by it like everything else.
--
-- Sign-in is username and password. There is no query here that looks a person
-- up by address, because an address no longer identifies anybody — it is a
-- contact field, and the credential is the password hash.

-- name: CreateUser :one
INSERT INTO users (username, password_hash, display_name, is_superuser)
VALUES (lower(@username::text), @password_hash, @display_name, @is_superuser)
RETURNING *;

-- Seeding the superuser is an upsert on the name, and it deliberately does not
-- touch password_hash. Re-running the process must not put the built-in default
-- back over a password somebody has since changed — that would make every
-- restart a silent credential reset.
-- name: EnsureSuperuser :one
INSERT INTO users (username, password_hash, display_name, is_superuser)
VALUES (lower(@username::text), @password_hash, @display_name, true)
ON CONFLICT (lower(username)) DO UPDATE
SET is_superuser = true,
    updated_at   = now()
RETURNING *;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE lower(username) = lower(@username::text);

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: ListUsers :many
SELECT * FROM users ORDER BY is_superuser DESC, lower(username);

-- name: SetUserPassword :exec
UPDATE users SET password_hash = @password_hash, updated_at = now() WHERE id = @id;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = @id AND NOT is_superuser;

-- name: CountSuperusers :one
SELECT count(*) FROM users WHERE is_superuser;

-- name: CreateTeam :one
INSERT INTO teams (id, display_name)
VALUES (@id, @display_name)
ON CONFLICT (id) DO UPDATE SET display_name = excluded.display_name, updated_at = now()
RETURNING *;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = @id;

-- A role change reads the owner count and then writes; taking the team row
-- first serialises those pairs, so two concurrent demotions cannot both see
-- two owners and both proceed.
-- name: LockTeam :one
SELECT * FROM teams WHERE id = @id FOR UPDATE;

-- name: UpsertMembership :one
INSERT INTO memberships (user_id, owner_id, role)
VALUES (@user_id, @owner_id, @role)
ON CONFLICT (user_id, owner_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- name: GetMembership :one
SELECT * FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: ListMembershipsForUser :many
SELECT m.*, t.display_name AS team_name
FROM memberships m
JOIN teams t ON t.id = m.owner_id
WHERE m.user_id = @user_id
ORDER BY t.display_name;

-- name: ListMembersOfTeam :many
SELECT m.*, u.username, u.display_name AS user_name
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.owner_id = @owner_id
ORDER BY lower(u.username);

-- name: DeleteMembership :exec
DELETE FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: CountOwnersOfTeam :one
SELECT count(*) FROM memberships WHERE owner_id = @owner_id AND role = 'owner';

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, active_team_id, user_agent, ip, expires_at)
VALUES (@user_id, @token_hash, @active_team_id, @user_agent, @ip, @expires_at)
RETURNING *;

-- The team is joined in because the request that carries this cookie needs the
-- owner it resolves to, and a second round trip per request buys nothing. The
-- join is LEFT: a session whose team was deleted still exists, it just has no
-- owner to act as.
--
-- Expiry is filtered in SQL rather than in Go so that an expired row can never
-- be treated as valid by a caller that forgets to check.
-- Resolves a session only while the membership behind it still exists.
--
-- The join to memberships is the security boundary, not decoration. Revoking
-- sessions imperatively when someone is removed would work only for the call
-- sites that remember to do it, and would still miss membership lost by
-- cascade. Joining makes a departed member's cookie stop resolving the instant
-- the row goes, by whatever route it went.
--
-- INNER JOIN on teams too: a session with no active team resolves to no owner,
-- and returning a row the caller must then remember to reject is how that
-- check gets skipped.
-- name: GetSessionByHash :one
SELECT s.*, t.display_name AS team_name, t.email AS team_email, m.role AS member_role
FROM sessions s
JOIN teams t ON t.id = s.active_team_id
JOIN memberships m ON m.owner_id = s.active_team_id AND m.user_id = s.user_id
WHERE s.token_hash = @token_hash AND s.expires_at > now();

-- name: GetSession :one
SELECT * FROM sessions WHERE id = @id;

-- name: SetSessionTeam :exec
UPDATE sessions SET active_team_id = @active_team_id WHERE id = @id;

-- name: DeleteSessionByHash :exec
DELETE FROM sessions WHERE token_hash = @token_hash;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = @user_id;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= now();
