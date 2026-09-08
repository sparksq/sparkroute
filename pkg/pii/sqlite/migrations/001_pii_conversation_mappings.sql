CREATE TABLE IF NOT EXISTS llm_pii_conversation_mappings (
    tenant_id TEXT NOT NULL CHECK (length(CAST(tenant_id AS BLOB)) BETWEEN 1 AND 256),
    principal_id TEXT NOT NULL CHECK (length(CAST(principal_id AS BLOB)) BETWEEN 1 AND 256),
    conversation_id TEXT NOT NULL CHECK (length(CAST(conversation_id AS BLOB)) BETWEEN 1 AND 256),
    entity TEXT NOT NULL CHECK (length(CAST(entity AS BLOB)) BETWEEN 1 AND 64),
    lookup_digest BLOB NOT NULL CHECK (length(lookup_digest) = 32),
    token TEXT NOT NULL CHECK (length(CAST(token AS BLOB)) BETWEEN 16 AND 256),
    key_id TEXT NOT NULL CHECK (length(CAST(key_id AS BLOB)) BETWEEN 1 AND 128),
    nonce BLOB NOT NULL CHECK (length(nonce) BETWEEN 12 AND 32),
    ciphertext BLOB NOT NULL,
    original_bytes INTEGER NOT NULL CHECK (original_bytes > 0),
    created_at_unix_ns INTEGER NOT NULL CHECK (created_at_unix_ns > 0),
    last_used_at_unix_ns INTEGER NOT NULL CHECK (last_used_at_unix_ns >= created_at_unix_ns),
    expires_at_unix_ns INTEGER NOT NULL CHECK (expires_at_unix_ns > created_at_unix_ns),
    absolute_expires_at_unix_ns INTEGER NOT NULL CHECK (absolute_expires_at_unix_ns >= expires_at_unix_ns),
    PRIMARY KEY (tenant_id, principal_id, conversation_id, lookup_digest),
    UNIQUE (tenant_id, principal_id, conversation_id, token)
);

CREATE INDEX IF NOT EXISTS llm_pii_conversation_mappings_expiry_idx
    ON llm_pii_conversation_mappings (expires_at_unix_ns);

CREATE INDEX IF NOT EXISTS llm_pii_conversation_mappings_absolute_expiry_idx
    ON llm_pii_conversation_mappings (absolute_expires_at_unix_ns);
