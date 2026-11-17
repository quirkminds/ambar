-- +goose Up

-- Something kept. It belongs to a vault; created_by is who kept it (kept even if they leave the vault).
-- sha256 is the fingerprint the client declared and processing confirmed: duplicates are found by it,
-- only within one vault. deleted_at set means it is in the Trash, where it still counts against the quota
-- until it is erased for good.
CREATE TABLE moments (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    vault_id     uuid NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    kind         text NOT NULL CHECK (kind IN ('photo', 'clip', 'link', 'file')),
    name         text NOT NULL DEFAULT '',
    content_type text NOT NULL,
    sha256       bytea NOT NULL,
    captured_at  timestamptz,
    width        integer,
    height       integer,
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

CREATE INDEX moments_fingerprint ON moments (vault_id, sha256);

-- The files of a moment in R2: the clean original and the copies made to be shown. Only the original
-- counts against the quota; the copies are Ambar's cost.
CREATE TABLE moment_files (
    moment_id    uuid NOT NULL REFERENCES moments (id) ON DELETE CASCADE,
    variant      text NOT NULL CHECK (variant IN ('original', 'small', 'large', 'display')),
    key          text NOT NULL UNIQUE,
    content_type text NOT NULL,
    size         bigint NOT NULL CHECK (size > 0),
    width        integer,
    height       integer,
    PRIMARY KEY (moment_id, variant)
);

-- Processing: a worker leases an upload (lease_until) and counts its attempts; an expired lease is
-- taken again, so a crash never strands one. moment_id is what a ready upload became.
ALTER TABLE uploads
    ADD COLUMN moment_id   uuid REFERENCES moments (id) ON DELETE SET NULL,
    ADD COLUMN attempts    integer NOT NULL DEFAULT 0,
    ADD COLUMN lease_until timestamptz;

CREATE INDEX uploads_to_process ON uploads (completed_at) WHERE status IN ('uploaded', 'processing');

-- +goose Down

DROP INDEX uploads_to_process;
ALTER TABLE uploads DROP COLUMN moment_id, DROP COLUMN attempts, DROP COLUMN lease_until;
DROP TABLE moment_files;
DROP TABLE moments;
