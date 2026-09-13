CREATE INDEX IF NOT EXISTS model_runtime_events_virtual_model_occurred_idx
    ON model_runtime_events (virtual_model, occurred_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS model_runtime_events_outcome_occurred_idx
    ON model_runtime_events (outcome, occurred_at DESC, event_id DESC);
