-- +goose Up

-- Everything a person keeps lives in a vault. Each user has one personal vault (personal_of); shared
-- vaults (later) have none. Ids are random v4: they reach clients and media URLs, and a v7 id would
-- reveal when the vault, and so its owner's account, was created.
CREATE TABLE vaults (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                text NOT NULL DEFAULT '',
    personal_of         uuid UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    -- Set by the API from the plan (Seed today); only the API changes it, never a client.
    storage_limit_bytes bigint NOT NULL CHECK (storage_limit_bytes >= 0),
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Who can do what in a vault, for people and (later) agents alike.
CREATE TABLE vault_members (
    vault_id   uuid NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'editor', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (vault_id, user_id)
);

CREATE INDEX vault_members_user ON vault_members (user_id);

-- Every existing account gets its personal vault, with the Seed plan's 2 GB.
INSERT INTO vaults (personal_of, storage_limit_bytes)
SELECT id, 2147483648 FROM users;

INSERT INTO vault_members (vault_id, user_id, role)
SELECT id, personal_of, 'owner' FROM vaults WHERE personal_of IS NOT NULL;

-- +goose Down

DROP TABLE vault_members;
DROP TABLE vaults;
