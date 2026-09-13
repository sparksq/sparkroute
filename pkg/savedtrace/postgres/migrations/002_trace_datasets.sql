ALTER TABLE llm_saved_traces ADD COLUMN conversation_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN response_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN parent_response_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN session_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN capture_session_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN capture_outcome TEXT NOT NULL DEFAULT 'complete';

UPDATE llm_saved_traces SET capture_outcome = 'truncated'
WHERE request_truncated OR response_truncated;
UPDATE llm_saved_traces SET capture_outcome = 'incomplete'
WHERE capture_outcome = 'complete' AND stream AND outcome = 'success'
  AND response_content_type = 'text/event-stream'
  AND response_body NOT LIKE '%data: [DONE]%'
  AND response_body NOT LIKE '%"type":"response.completed"%';

CREATE INDEX llm_saved_traces_conversation_started_idx
    ON llm_saved_traces (conversation_id, started_at DESC);
CREATE INDEX llm_saved_traces_session_started_idx
    ON llm_saved_traces (session_id, started_at DESC);
CREATE INDEX llm_saved_traces_capture_session_started_idx
    ON llm_saved_traces (capture_session_id, started_at DESC);

CREATE TABLE llm_saved_trace_capture_sessions (
    id TEXT PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    mode TEXT NOT NULL,
    accepted BIGINT NOT NULL,
    persisted BIGINT NOT NULL,
    pending BIGINT NOT NULL,
    lost BIGINT NOT NULL,
    invalid BIGINT NOT NULL,
    queue_full BIGINT NOT NULL,
    closed BIGINT NOT NULL,
    store_failures BIGINT NOT NULL
);

CREATE INDEX llm_saved_trace_capture_sessions_window_idx
    ON llm_saved_trace_capture_sessions (started_at, updated_at);
