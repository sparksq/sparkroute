CREATE TABLE IF NOT EXISTS model_runtime_events (
    event_id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    occurred_at_unix_ns INTEGER NOT NULL,
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
    latency_ns INTEGER,
    queue_depth INTEGER NOT NULL,
    waiter_count INTEGER NOT NULL,
    outcome TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS model_runtime_events_occurred_idx
    ON model_runtime_events (occurred_at_unix_ns DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_controller_seek_idx
    ON model_runtime_events (controller, occurred_at_unix_ns DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_virtual_model_seek_idx
    ON model_runtime_events (virtual_model, occurred_at_unix_ns DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_deployment_seek_idx
    ON model_runtime_events (deployment, occurred_at_unix_ns DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_outcome_seek_idx
    ON model_runtime_events (outcome, occurred_at_unix_ns DESC, event_id DESC);
