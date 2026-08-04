-- +goose Up
-- +goose StatementBegin

-- The command line that replaces the image's entrypoint, as it was typed.
--
-- Stored as the raw string rather than the parsed argv, so the form that edits
-- it shows back what somebody wrote — quotes and all — instead of a
-- re-quoted rendering of what it was understood to mean. Parsing is
-- deterministic, so the argv is recoverable at every apply; the typing is not.
--
-- Empty runs whatever the image already says to, which is every app that
-- existed before this column.
ALTER TABLE apps ADD COLUMN command text NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps DROP COLUMN IF EXISTS command;
-- +goose StatementEnd
