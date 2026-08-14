-- +goose Up
-- +goose StatementBegin

-- Username and password sign-in, replacing magic links and invitations.
--
-- What changes is how somebody proves who they are, not who they are. Users,
-- sessions, memberships and every owner_id in the schema stay exactly as they
-- were, which is why this migration touches four columns and two dead tables
-- rather than the ownership model underneath them.
--
-- An install now has a superuser seeded at startup, so there is no longer a
-- bootstrapping problem for mail to solve: the first way in does not depend on
-- an address, a relay, or a link that expires.

ALTER TABLE users ADD COLUMN username      text;
ALTER TABLE users ADD COLUMN password_hash bytea;

-- Superuser is a property of the person, not a role in a team.
--
-- Deliberately separate from memberships. A role says what somebody may do
-- with apps; this says who may create and remove the people who hold those
-- roles. Folding the two together would make "can deploy" and "can grant
-- another person the ability to deploy" the same permission, and they are not.
ALTER TABLE users ADD COLUMN is_superuser boolean NOT NULL DEFAULT false;

-- Existing rows keep their identity: the local part of the address is the
-- obvious username, and it is what the person already types.
UPDATE users SET username = split_part(email, '@', 1) WHERE username IS NULL;

-- Addresses stop being an identity and become an optional contact field, so
-- the uniqueness that enforced "one address, one person" moves to the username.
-- Compared case-insensitively for the same reason the address was: Batman and
-- batman are one person, and treating them as two is a second account nobody
-- can sign in to.
DROP INDEX users_email_key;
ALTER TABLE users ALTER COLUMN email DROP NOT NULL;
ALTER TABLE users ALTER COLUMN username SET NOT NULL;
CREATE UNIQUE INDEX users_username_key ON users (lower(username));

-- password_hash stays nullable, and that is the useful state rather than an
-- oversight: a row with no hash cannot sign in at all. Any user predating this
-- migration lands there, which is the correct answer for an account whose only
-- credential was an emailed link.

-- Both tables are dropped rather than left in place. A dormant invitations
-- table is a set of live tokens that no code path can revoke any more, and a
-- magic_links table nothing writes to is a schema that lies about how the
-- install authenticates.
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS magic_links;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The dropped tables are recreated as they were in 00003. Their contents are
-- not recoverable, which is the honest outcome: the tokens they held were
-- hashed and time-limited, and any that survived this round trip would have
-- expired regardless.
CREATE TABLE magic_links (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX magic_links_token_hash_key ON magic_links (token_hash);

CREATE TABLE invitations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    email       text        NOT NULL,
    role        text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    token_hash  bytea       NOT NULL,
    invited_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX invitations_token_hash_key ON invitations (token_hash);
CREATE INDEX invitations_owner_id_idx ON invitations (owner_id);
CREATE UNIQUE INDEX invitations_pending_key
    ON invitations (owner_id, lower(email)) WHERE accepted_at IS NULL;

DROP INDEX users_username_key;
ALTER TABLE users DROP COLUMN is_superuser;
ALTER TABLE users DROP COLUMN password_hash;
ALTER TABLE users DROP COLUMN username;

-- A row that arrived after this migration may have no address at all, so the
-- old NOT NULL cannot simply be reinstated. An empty string is what the column
-- meant before it was nullable.
UPDATE users SET email = '' WHERE email IS NULL;
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

-- +goose StatementEnd
