CREATE TABLE IF NOT EXISTS llm_saved_traces (
    request_id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    started_at_unix_ns INTEGER NOT NULL,
    completed_at TEXT NOT NULL,
    tenant_id TEXT,
    principal_id TEXT,
    principal_type TEXT,
    principal_subject TEXT,
    metadata_json TEXT NOT NULL,
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
    request_content_type TEXT,
    request_body TEXT NOT NULL,
    request_truncated INTEGER NOT NULL,
    response_content_type TEXT,
    response_body TEXT NOT NULL,
    response_truncated INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS llm_saved_traces_started_idx
    ON llm_saved_traces (started_at_unix_ns DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS llm_saved_traces_tenant_started_idx
    ON llm_saved_traces (tenant_id, started_at_unix_ns DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS llm_saved_traces_virtual_model_started_idx
    ON llm_saved_traces (virtual_model, started_at_unix_ns DESC);
CREATE INDEX IF NOT EXISTS llm_saved_traces_deployment_started_idx
    ON llm_saved_traces (final_deployment, started_at_unix_ns DESC);
