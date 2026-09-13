CREATE TABLE IF NOT EXISTS gateway_client_credentials (
    credential_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    name TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    principal_type TEXT NOT NULL DEFAULT '',
    principal_subject TEXT NOT NULL DEFAULT '',
    roles JSONB NOT NULL,
    allowed_attribution JSONB NOT NULL,
    fixed_attribution JSONB NOT NULL,
    secret_sha256 BYTEA NOT NULL CHECK (octet_length(secret_sha256) = 32),
    state TEXT NOT NULL CHECK (state IN ('active', 'disabled', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by TEXT NOT NULL,
    rotated_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_tenant_created_idx
    ON gateway_client_credentials (tenant_id, created_at DESC, credential_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_principal_created_idx
    ON gateway_client_credentials (principal_id, created_at DESC, credential_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credentials_state_created_idx
    ON gateway_client_credentials (state, created_at DESC, credential_id DESC);

CREATE TABLE IF NOT EXISTS gateway_client_credential_audit (
    event_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES gateway_client_credentials (credential_id),
    tenant_id TEXT NOT NULL,
    action TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor TEXT NOT NULL,
    details JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS gateway_client_credential_audit_tenant_event_idx
    ON gateway_client_credential_audit (tenant_id, event_id DESC);

CREATE INDEX IF NOT EXISTS gateway_client_credential_audit_credential_event_idx
    ON gateway_client_credential_audit (credential_id, event_id DESC);
