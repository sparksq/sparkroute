CREATE INDEX IF NOT EXISTS llm_requests_virtual_model_seek_idx
    ON llm_requests (virtual_model, started_at DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS llm_requests_outcome_seek_idx
    ON llm_requests (outcome, started_at DESC, request_id DESC);

CREATE INDEX IF NOT EXISTS llm_attempts_request_seek_idx
    ON llm_attempts (
        request_id,
        attempt_started_at DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_provider_seek_idx
    ON llm_attempts (
        provider,
        attempt_started_at DESC,
        request_id DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_deployment_seek_idx
    ON llm_attempts (
        deployment,
        attempt_started_at DESC,
        request_id DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_outcome_seek_idx
    ON llm_attempts (
        outcome,
        attempt_started_at DESC,
        request_id DESC,
        attempt_no DESC
    );
