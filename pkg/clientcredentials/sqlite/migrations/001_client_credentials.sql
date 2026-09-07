CREATE TABLE IF NOT EXISTS gateway_client_credentials (
    credential_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    principal_type TEXT NOT NULL DEFAULT '',
    principal_subject TEXT NOT NULL DEFAULT '',
    roles_json TEXT NOT NULL,
    allowed_attribution_json TEXT NOT NULL,
    fixed_attribution_json TEXT NOT NULL,
    secret_sha256 BLOB NOT NULL CHECK (length(secret_sha256) = 32),
    state TEXT NOT NULL CHECK (state IN ('active', 'disabled', 'revoked')),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    rotated_at TEXT,
    expires_at TEXT,
    revoked_at TEXT
);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_tenant_created_idx
    ON gateway_client_credentials (tenant_id, created_at DESC, credential_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_principal_created_idx
    ON gateway_client_credentials (principal_id, created_at DESC, credential_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_state_created_idx
    ON gateway_client_credentials (state, created_at DESC, credential_id DESC);

CREATE TABLE IF NOT EXISTS gateway_client_credential_audit (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    credential_id TEXT NOT NULL,
    tenant_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    actor TEXT NOT NULL,
    details_json TEXT NOT NULL,
    FOREIGN KEY (credential_id) REFERENCES gateway_client_credentials (credential_id)
);

CREATE INDEX IF NOT EXISTS gateway_client_credential_audit_tenant_event_idx
    ON gateway_client_credential_audit (tenant_id, event_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credential_audit_credential_event_idx
    ON gateway_client_credential_audit (credential_id, event_id DESC);
