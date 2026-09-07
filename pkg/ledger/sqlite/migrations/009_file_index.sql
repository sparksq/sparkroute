CREATE TABLE IF NOT EXISTS llm_file_index (
    owner_scope TEXT NOT NULL
        CHECK (length(owner_scope) BETWEEN 1 AND 128),
    resource_kind TEXT NOT NULL DEFAULT 'file'
        CHECK (resource_kind = 'file'),
    file_id TEXT NOT NULL
        CHECK (length(file_id) BETWEEN 1 AND 512),
    bytes INTEGER NOT NULL
        CHECK (bytes >= 0),
    created_at INTEGER NOT NULL
        CHECK (created_at >= 0),
    expires_at INTEGER,
    filename TEXT NOT NULL
        CHECK (length(CAST(filename AS BLOB)) <= 1024),
    purpose TEXT NOT NULL
        CHECK (length(CAST(purpose AS BLOB)) <= 256),
    CHECK (expires_at IS NULL OR expires_at > created_at),
    PRIMARY KEY (owner_scope, file_id),
    FOREIGN KEY (owner_scope, resource_kind, file_id)
        REFERENCES llm_resource_affinities (owner_scope, resource_kind, resource_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS llm_file_index_scope_created_idx
    ON llm_file_index (owner_scope, created_at DESC, file_id DESC);

CREATE INDEX IF NOT EXISTS llm_file_index_scope_purpose_created_idx
    ON llm_file_index (owner_scope, purpose, created_at DESC, file_id DESC);

-- Existing gateway-owned affinities predate indexed metadata. Preserve their
-- discoverability with bounded sparse records; subsequent uploads are indexed
-- atomically with the complete provider metadata available at creation time.
INSERT INTO llm_file_index (
    owner_scope, resource_kind, file_id, bytes, created_at,
    expires_at, filename, purpose
)
SELECT owner_scope, 'file', resource_id, 0, bound_at_unix_ns / 1000000000,
       NULL, '', ''
FROM llm_resource_affinities
WHERE resource_kind = 'file'
ON CONFLICT (owner_scope, file_id) DO NOTHING;
