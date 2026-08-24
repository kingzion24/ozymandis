-- +goose Up
-- +goose StatementBegin

-- The repository a project stands for.
--
-- Projects existed before this and had to be created by hand, so every app
-- landed in "Default" and the panel answered nothing: a list of nine apps in
-- one group does not say which system they belong to. The repository already
-- on each app is the answer people actually use — "that's the chatbot repo,
-- that's the dashboard repo" — so it becomes the project's identity.
--
-- Empty rather than null. A project somebody typed a name for is not a project
-- with a missing repository; it is a project that is not about one repository,
-- and "" says that without every read having to handle a null.
ALTER TABLE projects ADD COLUMN repo text NOT NULL DEFAULT '';

-- One project per repository, per team.
--
-- Partial, so the hand-made projects — every one of which has "" — do not
-- collide with each other. Keyed on the lowercased identity, because a forge
-- that treats "harehaDET/App" and "harehadet/app" as one repository would
-- otherwise get a project per spelling somebody happened to paste, while the
-- column keeps the spelling the owner actually uses for showing on screen.
CREATE UNIQUE INDEX projects_owner_repo_idx
    ON projects (owner_id, lower(repo)) WHERE repo <> '';

-- Backfill: give the apps already here the projects they would get today.
--
-- Done once, in the migration, rather than on every read. Adopting on read is
-- how the default project works, but it can only be right for apps that have
-- no project at all — repeat it for apps that have one and it would drag an
-- app back out of wherever somebody deliberately moved it, on the next page
-- load, forever.
--
-- So this touches only apps nobody has filed: no project, or the default one.
WITH identified AS (
    SELECT
        a.id       AS app_id,
        a.owner_id AS owner_id,
        -- The URL reduced to "host/owner/name", one layer at a time, reading
        -- outward from the innermost call: the scheme goes, then any "git@",
        -- then any ":port", then the ":" of an scp-style "host:owner/name"
        -- becomes the "/" it means — by this point a colon before the first
        -- slash can be nothing else — and finally any trailing ".git".
        --
        -- The same reduction Repo.Identity does in Go. They have to agree: if
        -- this wrote an identity the service would not recognise, the next
        -- deploy from that repository would make a second project beside the
        -- one this migration just filled.
        regexp_replace(
            regexp_replace(
                regexp_replace(
                    regexp_replace(
                        regexp_replace(a.repo_url, '^[A-Za-z0-9+.-]+://', ''),
                        '^[^/@]*@', ''),
                    '^([^/:]+):[0-9]+', '\1'),
                '^([^/:]+):', '\1/'),
            '\.git$', '') AS identity
    FROM apps a
    LEFT JOIN projects p ON p.id = a.project_id
    WHERE a.repo_url <> ''
      AND (a.project_id IS NULL OR p.slug = 'default')
),
named AS (
    SELECT
        app_id,
        owner_id,
        identity AS repo,
        -- The last path segment, as written: the case is how the owner spells
        -- the repository, and it is what the project gets called on screen.
        regexp_replace(identity, '^.*/', '') AS name,
        -- Everything after the host — "owner/name" — for the second address to
        -- try when two repositories want the first one.
        regexp_replace(identity, '^[^/]+/', '') AS path
    FROM identified
    -- A URL that reduced to nothing nameable is left alone rather than given a
    -- project called "".
    WHERE regexp_replace(identity, '^.*/', '') <> ''
),
-- One row per repository, not per app: six apps out of one repository are one
-- project, and the addresses below have to be decided once for the repository
-- rather than once for each app that happens to come from it.
repos AS (
    SELECT DISTINCT ON (owner_id, lower(repo)) owner_id, repo, name, path
    FROM named
    ORDER BY owner_id, lower(repo), name
),
slugged AS (
    SELECT
        r.*,
        -- The same rule Slugify applies in Go: lowercase, runs of anything
        -- else collapsed to one dash, dashes trimmed off both ends, cut to 63,
        -- then trimmed again in case the cut landed on one.
        trim(both '-' from left(
            trim(both '-' from regexp_replace(lower(r.name), '[^a-z0-9]+', '-', 'g')),
            63)) AS name_slug,
        trim(both '-' from left(
            trim(both '-' from regexp_replace(lower(r.path), '[^a-z0-9]+', '-', 'g')),
            63)) AS path_slug
    FROM repos r
),
ranked AS (
    SELECT
        s.*,
        row_number() OVER (PARTITION BY owner_id, name_slug ORDER BY lower(repo)) AS name_rank,
        row_number() OVER (PARTITION BY owner_id, path_slug ORDER BY lower(repo)) AS path_rank
    FROM slugged s
),
-- The address each repository will get.
--
-- Two different repositories can end in the same name — "acme/api" and
-- "beta/api" — and a team can already have a project sitting on that address.
-- The loser falls back to including the owner, which is still a name somebody
-- recognises, and is the same second choice ProjectForRepo makes in Go. A
-- repository that can get neither is left in the default project rather than
-- filed under an address nobody would guess.
projected AS (
    SELECT
        owner_id,
        repo,
        name,
        CASE
            WHEN name_slug <> '' AND name_rank = 1 AND NOT EXISTS (
                SELECT 1 FROM projects x
                WHERE x.owner_id = ranked.owner_id AND x.slug = ranked.name_slug)
            THEN name_slug
            WHEN path_slug <> '' AND path_rank = 1 AND NOT EXISTS (
                SELECT 1 FROM projects x
                WHERE x.owner_id = ranked.owner_id AND x.slug = ranked.path_slug)
            THEN path_slug
        END AS slug
    FROM ranked
),
created AS (
    INSERT INTO projects (owner_id, slug, name, repo)
    SELECT owner_id, slug, left(name, 100), repo
    FROM projected
    WHERE slug IS NOT NULL
    RETURNING id, owner_id, repo
)
UPDATE apps a
SET project_id = c.id, updated_at = now(),
    -- The default project's arrangement means nothing on the new canvas, and a
    -- card left at its old coordinates lands on top of another one. Cleared,
    -- the next read lays each project out from its own dependencies.
    canvas_x = NULL, canvas_y = NULL
FROM created c, named n
WHERE n.app_id = a.id AND n.owner_id = c.owner_id AND lower(n.repo) = lower(c.repo);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Put the apps back where they were before dropping the projects that hold
-- them, so a rollback does not leave them unassigned and waiting for the
-- default project to adopt them one page load later.
UPDATE apps a
SET project_id = (SELECT p.id FROM projects p WHERE p.owner_id = a.owner_id AND p.slug = 'default')
WHERE a.project_id IN (SELECT id FROM projects WHERE repo <> '');

DELETE FROM projects WHERE repo <> '';

DROP INDEX IF EXISTS projects_owner_repo_idx;
ALTER TABLE projects DROP COLUMN IF EXISTS repo;

-- +goose StatementEnd
