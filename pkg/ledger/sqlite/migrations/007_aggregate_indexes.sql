CREATE INDEX IF NOT EXISTS llm_requests_final_provider_started_idx
    ON llm_requests (final_provider, started_at_unix_ns DESC);
CREATE INDEX IF NOT EXISTS llm_requests_final_deployment_started_idx
    ON llm_requests (final_deployment, started_at_unix_ns DESC);
