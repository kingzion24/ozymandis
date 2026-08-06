-- +goose Up
-- +goose StatementBegin

-- Every shell anybody opened into a container.
--
-- Its own table rather than a row in the deploy feed: a session is not a
-- deployment and shares none of its columns. It is here because an interactive
-- shell in a production container is the most sensitive thing this platform can
-- do — it reads every secret the app holds and leaves no trace in the app's own
-- logs — so it should be the easiest thing to see afterwards.
--
-- # One row, written twice
--
-- The design calls for two events, a start and an end. This is one row inserted
-- at the start and updated at the end, which produces the identical observable
-- property with less machinery: no correlation between two rows, and no way for
-- the pair to disagree.
--
-- What matters is the ordering, and it is load-bearing in both directions:
--
--   * The INSERT happens BEFORE the stream opens. A session that dies during
--     attach — a refused upgrade, a pod that went away between the check and
--     the dial — has already been recorded. Recording at teardown instead would
--     leave nothing at all for exactly the sessions that failed strangely.
--
--   * The UPDATE happens on a detached, bounded context. A WebSocket client
--     vanishing is the NORMAL end of a session, not the exception, so the
--     request context is usually already cancelled by the very disconnect that
--     triggered the teardown. Writing through it would fail precisely for the
--     sessions most worth recording.
--
-- A row with ended_at IS NULL is therefore either a session still running or
-- one that ended in a way this process never observed. Both are worth seeing,
-- and neither is distinguishable from the other by anything we could have
-- recorded — which is why the column is nullable rather than defaulted.
CREATE TABLE exec_sessions (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id text NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id   uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- Who opened it. Free text rather than a users reference: the actor may be
    -- an API token's user, a session's user, or the single owner of an install
    -- with no accounts at all, and an audit row that could not be written
    -- because the actor did not fit a foreign key would be an audit row lost.
    actor text NOT NULL DEFAULT '',

    -- Which container, and what was run in it. The command is the whole point:
    -- "somebody opened a shell" is an alert, "somebody ran psql" is an answer.
    pod     text NOT NULL,
    command text NOT NULL,
    tty     boolean NOT NULL DEFAULT false,

    started_at timestamptz NOT NULL DEFAULT now(),

    -- NULL while the session runs, and forever if it ended unobserved.
    ended_at timestamptz,

    -- How it ended. Empty while running.
    --
    --   exited        the command finished, and exit_code says with what
    --   disconnected  the client went away mid-session
    --   failed        the session could not be established or did not survive
    --
    -- "disconnected" is the one that matters: a shell killed by a network blip
    -- and one that ran something and exited cleanly are different events, and a
    -- schema that could not tell them apart would make the first invisible.
    outcome   text NOT NULL DEFAULT ''
              CHECK (outcome IN ('', 'exited', 'disconnected', 'failed')),
    exit_code integer
);

CREATE INDEX exec_sessions_owner_started_idx
    ON exec_sessions (owner_id, started_at DESC);

-- Finding the sessions that never closed, which is the signal this table
-- exists to make visible.
CREATE INDEX exec_sessions_open_idx
    ON exec_sessions (owner_id) WHERE ended_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE exec_sessions;
-- +goose StatementEnd
