CREATE TABLE IF NOT EXISTS llm_resource_affinities (
    owner_scope TEXT NOT NULL
        CHECK (octet_length(owner_scope) BETWEEN 1 AND 128),
    resource_kind TEXT NOT NULL
        CHECK (resource_kind IN ('response', 'conversation', 'item', 'file', 'prompt')),
    resource_id TEXT NOT NULL
        CHECK (octet_length(resource_id) BETWEEN 1 AND 512),
    virtual_model TEXT NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    bound_at TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    CHECK (deleted_at IS NULL OR expires_at IS NOT NULL),
    CHECK (expires_at IS NULL OR expires_at > bound_at),
    CHECK (deleted_at IS NULL OR expires_at > deleted_at),
    PRIMARY KEY (owner_scope, resource_kind, resource_id)
);

CREATE INDEX IF NOT EXISTS llm_resource_affinities_expiry_idx
    ON llm_resource_affinities (expires_at)
    WHERE expires_at IS NOT NULL;
