-- +goose Up
-- +goose StatementBegin

-- The command that runs against a new image before traffic moves to it.
--
-- The gap this closes: a deploy goes build -> apply with nothing between, so
-- the second deploy of anything with a database is a race between a migration
-- nobody ran and an image that assumes it did. Putting the hook here rather
-- than asking people to run migrations from their entrypoint matters because an
-- entrypoint migration runs once per replica, concurrently, on every restart —
-- which is how two pods run the same migration at the same time.
--
-- Empty means no release step, which is the right default: most apps have
-- nothing to run, and a column defaulting to some guessed command would be a
-- deploy doing something nobody asked for.
ALTER TABLE apps ADD COLUMN release_command text NOT NULL DEFAULT '';

-- What the release did, per deployment.
--
-- On the deployment rather than the app, because the question is always about
-- one attempt: "did the migrations run for THIS deploy" is what somebody asks
-- at 3am, and an app-level column would only ever hold the answer for the most
-- recent one.
ALTER TABLE deployments ADD COLUMN release_log text NOT NULL DEFAULT '';

-- A distinct column rather than something inferred from an empty log.
--
-- "The release printed nothing" and "there was no release" are different
-- answers to *did my migrations run*, and a log-emptiness check would report
-- the first as the second — which is the reading that gets somebody to promote
-- a build whose migration silently never ran.
--
-- The four values are exhaustive on purpose:
--   ''            no deploy has run since this column existed
--   'skipped'     no release command is configured
--   'succeeded'   it ran and exited zero
--   'failed'      it ran and did not, and the deploy was vetoed
--   'unavailable' this orchestrator cannot run tasks, so it could not be tried
ALTER TABLE deployments ADD COLUMN release_status text NOT NULL DEFAULT ''
    CHECK (release_status IN ('', 'skipped', 'succeeded', 'failed', 'unavailable'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments DROP COLUMN release_status;
ALTER TABLE deployments DROP COLUMN release_log;
ALTER TABLE apps DROP COLUMN release_command;
-- +goose StatementEnd
