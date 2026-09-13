CREATE TABLE IF NOT EXISTS llm_runtime_activation_claims (
    activation_key TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    fencing_token BIGINT NOT NULL CHECK (fencing_token > 0),
    claim_state TEXT NOT NULL CHECK (claim_state IN ('active', 'completed')),
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS llm_runtime_activation_claims_expiry_idx
    ON llm_runtime_activation_claims (claim_state, expires_at);

CREATE TABLE IF NOT EXISTS llm_runtime_endpoints (
    endpoint_id TEXT PRIMARY KEY,
    target TEXT NOT NULL,
    base_url TEXT NOT NULL,
    protocol TEXT NOT NULL DEFAULT '',
    served_models_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    controller TEXT NOT NULL DEFAULT '',
    cluster_id TEXT NOT NULL DEFAULT '',
    job_id TEXT NOT NULL DEFAULT '',
    binding_revision TEXT NOT NULL DEFAULT '',
    recipe_revision TEXT NOT NULL DEFAULT '',
    fencing_token BIGINT NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    lifecycle_state TEXT NOT NULL CHECK (
        lifecycle_state IN ('offline', 'activating', 'ready', 'draining', 'deactivating', 'failed', 'unknown')
    ),
    active_requests INTEGER NOT NULL DEFAULT 0 CHECK (active_requests >= 0),
    max_concurrency INTEGER NOT NULL DEFAULT 0 CHECK (max_concurrency >= 0),
    registered_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS llm_runtime_endpoints_target_ready_idx
    ON llm_runtime_endpoints (target, lifecycle_state, endpoint_id);
CREATE INDEX IF NOT EXISTS llm_runtime_endpoints_controller_state_idx
    ON llm_runtime_endpoints (controller, lifecycle_state, endpoint_id);
CREATE INDEX IF NOT EXISTS llm_runtime_endpoints_expiry_idx
    ON llm_runtime_endpoints (expires_at)
    WHERE expires_at IS NOT NULL;
