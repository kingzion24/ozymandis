-- +goose Up
-- +goose StatementBegin

-- Credentials a person carries outside a browser.
--
-- A fourth thing that authenticates, alongside sessions, magic links and the
-- single shared OZYMANDIS_AUTH_TOKEN. It exists because the other three each
-- close a door the CLI needs open: a session is a cookie with a browser-shaped
-- TTL, a magic link is spent on sight, and the shared token only exists on a
-- single-owner install — the moment OZYMANDIS_BASE_URL turns sign-in on, the
-- identity provider is session-backed and there is no credential a script can
-- hold at all.
--
-- Scoped to a (user, team) pair rather than to a user. Everything downstream is
-- scoped by owner_id and owner_id means team, exactly as it does for a session:
-- a token that resolved to the person would silently widen every query to every
-- team they belong to. Someone in three teams mints three tokens, and each one
-- can reach precisely one of them.
CREATE TABLE api_tokens (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The team this token acts as. Named owner_id rather than team_id to match
    -- every other table in the schema, for the reason 00003 gives: the columns
    -- stayed owner_id through the teams rename so that no query had to change.
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,

    -- What it is for, so a token found in a CI log can be recognised and
    -- revoked without having to guess which one it is. Required, because a
    -- list of four credentials called "" is a list nobody will ever prune.
    name text NOT NULL CHECK (name <> ''),

    -- The hash, never the token. Same rule sessions, magic links and
    -- invitations follow, and for the same reason: a database dump must not be
    -- a set of working credentials.
    token_hash bytea NOT NULL,

    -- Best-effort, and nullable because a token that has never been used is a
    -- different fact from one used at the epoch. This is what makes an unused
    -- credential visible enough to delete.
    last_used_at timestamptz,

    -- Nullable on purpose: a token that never expires is the right default for
    -- a deploy key sitting in CI, and forcing an expiry on one only guarantees
    -- a pipeline that breaks on a date nobody wrote down. An expiry is offered
    -- rather than imposed.
    expires_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),

    -- The token belongs to the MEMBERSHIP, not merely to the user and the team
    -- separately. This composite reference is what makes removing somebody
    -- actually revoke their credentials rather than suspend them.
    --
    -- Without it the row survives: reads join memberships, so a departed
    -- member's token stops resolving and everything looks correct — until they
    -- are re-added months later and every credential they ever held silently
    -- comes back to life. Removal is the action an operator takes when a laptop
    -- is stolen, and "the token works again if they ever rejoin" is not what
    -- anybody believes they are doing when they press it.
    --
    -- Sessions have the same shape and get away with it because they expire; a
    -- token may have no expiry at all, so the suspension would be indefinite
    -- and then lift without a word.
    --
    -- Declared as a constraint rather than performed in RemoveMember, for the
    -- reason the identity middleware exists in one place: a cleanup every call
    -- site has to remember is one some call site will forget, and membership
    -- also goes by routes no service method sees — a cascaded user or team
    -- delete among them.
    FOREIGN KEY (user_id, owner_id)
        REFERENCES memberships (user_id, owner_id) ON DELETE CASCADE
);

-- Unique so that two tokens can never hash alike, which would make revoking
-- one silently revoke the other.
CREATE UNIQUE INDEX api_tokens_token_hash_key ON api_tokens (token_hash);

-- The list view is per person, per team.
CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);
CREATE INDEX api_tokens_owner_id_idx ON api_tokens (owner_id);

-- One name per person per team. Minting "ci" twice leaves two credentials that
-- read identically in the list, and revoking the wrong one breaks the pipeline
-- while leaving the leaked token live.
CREATE UNIQUE INDEX api_tokens_user_owner_name_key
    ON api_tokens (user_id, owner_id, lower(name));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE api_tokens;
-- +goose StatementEnd
