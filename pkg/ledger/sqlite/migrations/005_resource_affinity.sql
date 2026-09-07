CREATE TABLE IF NOT EXISTS llm_resource_affinities (
    owner_scope TEXT NOT NULL
        CHECK (length(owner_scope) BETWEEN 1 AND 128),
    resource_kind TEXT NOT NULL
        CHECK (resource_kind IN ('response', 'conversation', 'item', 'file', 'prompt')),
    resource_id TEXT NOT NULL
        CHECK (length(resource_id) BETWEEN 1 AND 512),
    virtual_model TEXT NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    bound_at TEXT NOT NULL,
    bound_at_unix_ns INTEGER NOT NULL,
    deleted_at TEXT,
    deleted_at_unix_ns INTEGER,
    expires_at TEXT,
    expires_at_unix_ns INTEGER,
    CHECK (
        (deleted_at IS NULL AND deleted_at_unix_ns IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_at_unix_ns IS NOT NULL)
    ),
    CHECK (
        (expires_at IS NULL AND expires_at_unix_ns IS NULL) OR
        (expires_at IS NOT NULL AND expires_at_unix_ns IS NOT NULL)
    ),
    CHECK (deleted_at IS NULL OR expires_at IS NOT NULL),
    PRIMARY KEY (owner_scope, resource_kind, resource_id)
);

CREATE INDEX IF NOT EXISTS llm_resource_affinities_expiry_idx
    ON llm_resource_affinities (expires_at_unix_ns)
    WHERE expires_at_unix_ns IS NOT NULL;
