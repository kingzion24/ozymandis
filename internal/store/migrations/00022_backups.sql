-- +goose Up
-- +goose StatementBegin

-- Where backups are written.
--
-- One row per owner, and off the machine on purpose. A K3s install stores its
-- volumes on local-path by default: the data lives on one node's disk, with no
-- replication and nothing between it and a failed disk. A snapshot kept on that
-- same disk is not a backup, it is a second copy of the thing being lost.
--
-- S3-compatible rather than a provider: the protocol is what R2, B2, Wasabi,
-- MinIO and S3 itself all speak, so choosing one is configuration rather than
-- code. There is deliberately no local filesystem option — it would be the
-- easiest one to pick and the one that protects against nothing.
CREATE TABLE backup_destinations (
    owner_id   text        PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,

    -- The S3 endpoint, such as https://<account>.r2.cloudflarestorage.com.
    -- Required even for AWS itself, because there is no default that is right
    -- for the other four.
    endpoint   text        NOT NULL CHECK (endpoint <> ''),
    bucket     text        NOT NULL CHECK (bucket <> ''),

    -- A prefix inside the bucket, so one bucket can hold several installs.
    -- Empty is the bucket root, which is right for a bucket made for this.
    prefix     text        NOT NULL DEFAULT '',

    -- Some providers ignore this and some require it. Kept rather than derived
    -- because "auto" is correct for R2 and wrong for S3.
    region     text        NOT NULL DEFAULT 'auto',

    access_key_id      text NOT NULL CHECK (access_key_id <> ''),

    -- Sealed with OZYMANDIS_SECRET_KEY, like every other credential this engine
    -- holds. Doubly so here: this one opens the place the backups are kept, so
    -- a readable copy in a database dump would put the dump and everything it
    -- was ever backed up alongside in one blast radius.
    secret_access_key  bytea NOT NULL,

    -- The restic repository password. Restic encrypts client-side, so this is
    -- what stands between the storage provider and the contents.
    --
    -- Losing it loses every snapshot, with no recovery path — the same property
    -- OZYMANDIS_SECRET_KEY has, and the reason this is sealed with that key rather
    -- than being another thing to keep safe separately.
    repo_password      bytea NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- What gets backed up, and how often.
--
-- Per app rather than install-wide, because the answer differs: a database
-- wants a nightly dump and a stateless web app has nothing worth keeping.
CREATE TABLE backup_policies (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    app_id     uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    enabled    boolean     NOT NULL DEFAULT true,

    -- A five-field cron expression, in the cluster's timezone. Kubernetes
    -- parses it, so the constraint here only rejects the shapes that are
    -- obviously not one — a full validation would be a second parser that has
    -- to agree with the first.
    schedule   text        NOT NULL DEFAULT '17 3 * * *'
                           CHECK (schedule ~ '^[-0-9*/, A-Za-z?]+( +[-0-9*/, A-Za-z?]+){4}$'),

    -- How many to keep, by age band, passed to `restic forget`.
    --
    -- Retention is applied by the backup job rather than by a separate sweep,
    -- because a pruning schedule that can fail independently is one that
    -- silently stops and is noticed when the bucket bill arrives.
    keep_daily   integer   NOT NULL DEFAULT 7  CHECK (keep_daily >= 0),
    keep_weekly  integer   NOT NULL DEFAULT 4  CHECK (keep_weekly >= 0),
    keep_monthly integer   NOT NULL DEFAULT 6  CHECK (keep_monthly >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- At least one band must keep something. All zeroes tells `restic forget`
    -- that nothing qualifies, and it deletes every snapshot it just made —
    -- a policy that reads as "keep nothing" and behaves as "destroy the
    -- backups", which is not a setting anybody should be able to save.
    CONSTRAINT backup_policies_keep_something
        CHECK (keep_daily + keep_weekly + keep_monthly > 0)
);

-- One policy per app: two would be two schedules writing the same repository.
CREATE UNIQUE INDEX backup_policies_app_key ON backup_policies (app_id);
CREATE INDEX backup_policies_owner_id_idx ON backup_policies (owner_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_policies;
DROP TABLE IF EXISTS backup_destinations;
-- +goose StatementEnd
