CREATE TABLE IF NOT EXISTS llm_pii_conversation_mappings (
    tenant_id TEXT NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 256),
    principal_id TEXT NOT NULL CHECK (octet_length(principal_id) BETWEEN 1 AND 256),
    conversation_id TEXT NOT NULL CHECK (octet_length(conversation_id) BETWEEN 1 AND 256),
    entity TEXT NOT NULL CHECK (octet_length(entity) BETWEEN 1 AND 64),
    lookup_digest BYTEA NOT NULL CHECK (octet_length(lookup_digest) = 32),
    token TEXT NOT NULL CHECK (octet_length(token) BETWEEN 16 AND 256),
    key_id TEXT NOT NULL CHECK (octet_length(key_id) BETWEEN 1 AND 128),
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) BETWEEN 12 AND 32),
    ciphertext BYTEA NOT NULL,
    original_bytes INTEGER NOT NULL CHECK (original_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL CHECK (last_used_at >= created_at),
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > created_at),
    absolute_expires_at TIMESTAMPTZ NOT NULL CHECK (absolute_expires_at >= expires_at),
    PRIMARY KEY (tenant_id, principal_id, conversation_id, lookup_digest),
    UNIQUE (tenant_id, principal_id, conversation_id, token)
);

CREATE INDEX IF NOT EXISTS llm_pii_conversation_mappings_expiry_idx
    ON llm_pii_conversation_mappings (expires_at);

CREATE INDEX IF NOT EXISTS llm_pii_conversation_mappings_absolute_expiry_idx
    ON llm_pii_conversation_mappings (absolute_expires_at);
