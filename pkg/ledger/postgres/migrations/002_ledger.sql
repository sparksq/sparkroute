CREATE TABLE IF NOT EXISTS llm_requests (
    started_at TIMESTAMPTZ NOT NULL,
    request_id TEXT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    protocol TEXT NOT NULL,
    operation TEXT NOT NULL,
    stream BOOLEAN NOT NULL,
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
    latency_ns BIGINT NOT NULL,
    time_to_first_byte_ns BIGINT,
    input_tokens BIGINT,
    output_tokens BIGINT,
    total_tokens BIGINT,
    cached_input_tokens BIGINT,
    cache_creation_tokens BIGINT,
    reasoning_tokens BIGINT,
    tool_use_prompt_tokens BIGINT,
    accepted_prediction_tokens BIGINT,
    rejected_prediction_tokens BIGINT,
    provider_components_json JSONB,
    raw_usage_json JSONB,
    usage_completeness TEXT NOT NULL,
    normalization_version TEXT,
    PRIMARY KEY (started_at, request_id)
);

CREATE INDEX IF NOT EXISTS llm_requests_request_id_started_idx
    ON llm_requests (request_id, started_at DESC);
CREATE INDEX IF NOT EXISTS llm_requests_virtual_model_started_idx
    ON llm_requests (virtual_model, started_at DESC);
CREATE INDEX IF NOT EXISTS llm_requests_outcome_started_idx
    ON llm_requests (outcome, started_at DESC);

CREATE TABLE IF NOT EXISTS llm_request_lookup (
    request_id TEXT PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_attempts (
    attempt_started_at TIMESTAMPTZ NOT NULL,
    request_id TEXT NOT NULL,
    attempt_no INTEGER NOT NULL,
    first_byte_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    pool_priority INTEGER NOT NULL,
    http_status INTEGER,
    upstream_request_id TEXT,
    outcome TEXT NOT NULL,
    failure_class TEXT,
    retried BOOLEAN NOT NULL,
    latency_ns BIGINT NOT NULL,
    input_tokens BIGINT,
    output_tokens BIGINT,
    total_tokens BIGINT,
    cached_input_tokens BIGINT,
    cache_creation_tokens BIGINT,
    reasoning_tokens BIGINT,
    tool_use_prompt_tokens BIGINT,
    accepted_prediction_tokens BIGINT,
    rejected_prediction_tokens BIGINT,
    provider_components_json JSONB,
    raw_usage_json JSONB,
    usage_completeness TEXT NOT NULL,
    normalization_version TEXT,
    PRIMARY KEY (attempt_started_at, request_id, attempt_no)
);

CREATE INDEX IF NOT EXISTS llm_attempts_request_attempt_started_idx
    ON llm_attempts (request_id, attempt_no, attempt_started_at DESC);
CREATE INDEX IF NOT EXISTS llm_attempts_deployment_started_idx
    ON llm_attempts (deployment, attempt_started_at DESC);
CREATE INDEX IF NOT EXISTS llm_attempts_outcome_started_idx
    ON llm_attempts (outcome, attempt_started_at DESC);

CREATE TABLE IF NOT EXISTS llm_attempt_lookup (
    request_id TEXT NOT NULL,
    attempt_no INTEGER NOT NULL,
    attempt_started_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (request_id, attempt_no)
);

CREATE TABLE IF NOT EXISTS model_runtime_events (
    occurred_at TIMESTAMPTZ NOT NULL,
    event_id TEXT NOT NULL,
    controller TEXT NOT NULL,
    binding_revision TEXT,
    virtual_model TEXT,
    deployment TEXT,
    endpoint_instance TEXT,
    cluster_id TEXT,
    job_id TEXT,
    recipe_revision TEXT,
    prior_state TEXT,
    new_state TEXT NOT NULL,
    reason TEXT,
    latency_ns BIGINT,
    queue_depth INTEGER,
    waiter_count INTEGER,
    outcome TEXT NOT NULL,
    metadata_json JSONB,
    PRIMARY KEY (occurred_at, event_id)
);

CREATE INDEX IF NOT EXISTS model_runtime_events_controller_occurred_idx
    ON model_runtime_events (controller, occurred_at DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_deployment_occurred_idx
    ON model_runtime_events (deployment, occurred_at DESC);
