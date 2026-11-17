-- +goose Up

-- A file on its way to becoming a moment. The bytes go from the client straight to R2 at
-- incoming/{id}; this row holds what was declared, the reservation against the vault's quota and where
-- the upload is. size and sha256 describe the bytes the client said it would send.
CREATE TABLE uploads (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    vault_id     uuid NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    content_type text NOT NULL,
    size         bigint NOT NULL CHECK (size > 0),
    sha256       bytea NOT NULL,
    captured_at  timestamptz,
    width        integer,
    height       integer,
    duration_ms  integer,
    status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'uploaded', 'processing', 'ready', 'failed', 'expired')),
    failure_code text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    completed_at timestamptz
);

-- Bytes reserved per vault: uploads that hold quota until they become moments or are released.
CREATE INDEX uploads_reserving ON uploads (vault_id) WHERE status IN ('pending', 'uploaded', 'processing');

-- Cleanup finds pending uploads whose slot ran out.
CREATE INDEX uploads_pending_expiry ON uploads (expires_at) WHERE status = 'pending';

-- +goose Down

DROP TABLE uploads;
