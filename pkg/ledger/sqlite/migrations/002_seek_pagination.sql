ALTER TABLE llm_requests
    ADD COLUMN started_at_unix_ns INTEGER NOT NULL DEFAULT 0;

UPDATE llm_requests
SET started_at_unix_ns =
    CAST(strftime('%s', started_at) AS INTEGER) * 1000000000 +
    CAST(substr(strftime('%f', started_at), 4, 3) AS INTEGER) * 1000000
WHERE started_at_unix_ns = 0;

CREATE INDEX IF NOT EXISTS llm_requests_seek_idx
    ON llm_requests (started_at_unix_ns DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS llm_requests_virtual_model_seek_idx
    ON llm_requests (virtual_model, started_at_unix_ns DESC, request_id DESC);
CREATE INDEX IF NOT EXISTS llm_requests_outcome_seek_idx
    ON llm_requests (outcome, started_at_unix_ns DESC, request_id DESC);

ALTER TABLE llm_attempts
    ADD COLUMN started_at_unix_ns INTEGER NOT NULL DEFAULT 0;

UPDATE llm_attempts
SET started_at_unix_ns =
    CAST(strftime('%s', started_at) AS INTEGER) * 1000000000 +
    CAST(substr(strftime('%f', started_at), 4, 3) AS INTEGER) * 1000000
WHERE started_at_unix_ns = 0;

CREATE INDEX IF NOT EXISTS llm_attempts_seek_idx
    ON llm_attempts (
        started_at_unix_ns DESC,
        request_id DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_request_seek_idx
    ON llm_attempts (
        request_id,
        started_at_unix_ns DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_provider_seek_idx
    ON llm_attempts (
        provider,
        started_at_unix_ns DESC,
        request_id DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_deployment_seek_idx
    ON llm_attempts (
        deployment,
        started_at_unix_ns DESC,
        request_id DESC,
        attempt_no DESC
    );
CREATE INDEX IF NOT EXISTS llm_attempts_outcome_seek_idx
    ON llm_attempts (
        outcome,
        started_at_unix_ns DESC,
        request_id DESC,
        attempt_no DESC
    );
