CREATE TABLE IF NOT EXISTS ledger_schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_requests (
    request_id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    completed_at TEXT NOT NULL,
    protocol TEXT NOT NULL,
    operation TEXT NOT NULL,
    stream INTEGER NOT NULL,
    requested_model TEXT,
    virtual_model TEXT,
    response_presented_model TEXT,
    config_revision TEXT,
    final_provider TEXT,
    final_deployment TEXT,
    final_upstream_model TEXT,
    attempt_count INTEGER NOT NULL,
    http_status INTEGER,
    outcome TEXT NOT NULL,
    failure_class TEXT,
    latency_ns INTEGER NOT NULL,
    time_to_first_byte_ns INTEGER,
    input_tokens INTEGER,
    output_tokens INTEGER,
    total_tokens INTEGER,
    cached_input_tokens INTEGER,
    cache_creation_tokens INTEGER,
    reasoning_tokens INTEGER,
    tool_use_prompt_tokens INTEGER,
    accepted_prediction_tokens INTEGER,
    rejected_prediction_tokens INTEGER,
    provider_components_json TEXT,
    raw_usage_json TEXT,
    usage_completeness TEXT NOT NULL,
    normalization_version TEXT
);

CREATE INDEX IF NOT EXISTS llm_requests_started_at_idx
    ON llm_requests (started_at);
CREATE INDEX IF NOT EXISTS llm_requests_virtual_model_started_idx
    ON llm_requests (virtual_model, started_at);
CREATE INDEX IF NOT EXISTS llm_requests_outcome_started_idx
    ON llm_requests (outcome, started_at);

CREATE TABLE IF NOT EXISTS llm_attempts (
    request_id TEXT NOT NULL,
    attempt_no INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    first_byte_at TEXT,
    completed_at TEXT NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    pool_priority INTEGER NOT NULL,
    http_status INTEGER,
    upstream_request_id TEXT,
    outcome TEXT NOT NULL,
    failure_class TEXT,
    retried INTEGER NOT NULL,
    latency_ns INTEGER NOT NULL,
    input_tokens INTEGER,
    output_tokens INTEGER,
    total_tokens INTEGER,
    cached_input_tokens INTEGER,
    cache_creation_tokens INTEGER,
    reasoning_tokens INTEGER,
    tool_use_prompt_tokens INTEGER,
    accepted_prediction_tokens INTEGER,
    rejected_prediction_tokens INTEGER,
    provider_components_json TEXT,
    raw_usage_json TEXT,
    usage_completeness TEXT NOT NULL,
    normalization_version TEXT,
    PRIMARY KEY (request_id, attempt_no)
);

CREATE INDEX IF NOT EXISTS llm_attempts_started_at_idx
    ON llm_attempts (started_at);
CREATE INDEX IF NOT EXISTS llm_attempts_deployment_started_idx
    ON llm_attempts (deployment, started_at);
CREATE INDEX IF NOT EXISTS llm_attempts_outcome_started_idx
    ON llm_attempts (outcome, started_at);
