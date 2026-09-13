SELECT create_hypertable(
    'llm_requests',
    'started_at',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists => TRUE
);

SELECT create_hypertable(
    'llm_attempts',
    'attempt_started_at',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists => TRUE
);

SELECT create_hypertable(
    'model_runtime_events',
    'occurred_at',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists => TRUE
);
