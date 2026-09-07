ALTER TABLE llm_saved_traces ADD COLUMN conversation_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN response_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN parent_response_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN session_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN capture_session_id TEXT;
ALTER TABLE llm_saved_traces ADD COLUMN capture_outcome TEXT NOT NULL DEFAULT 'complete';

UPDATE llm_saved_traces SET capture_outcome = 'truncated'
WHERE request_truncated = 1 OR response_truncated = 1;
UPDATE llm_saved_traces SET capture_outcome = 'incomplete'
WHERE capture_outcome = 'complete' AND stream = 1 AND outcome = 'success'
  AND response_content_type = 'text/event-stream'
  AND response_body NOT LIKE '%data: [DONE]%'
  AND response_body NOT LIKE '%"type":"response.completed"%';

CREATE INDEX llm_saved_traces_conversation_started_idx
    ON llm_saved_traces (conversation_id, started_at_unix_ns DESC);
CREATE INDEX llm_saved_traces_session_started_idx
    ON llm_saved_traces (session_id, started_at_unix_ns DESC);
CREATE INDEX llm_saved_traces_capture_session_started_idx
    ON llm_saved_traces (capture_session_id, started_at_unix_ns DESC);

CREATE TABLE llm_saved_trace_capture_sessions (
    id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    started_at_unix_ns INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    updated_at_unix_ns INTEGER NOT NULL,
    completed_at TEXT,
    mode TEXT NOT NULL,
    accepted INTEGER NOT NULL,
    persisted INTEGER NOT NULL,
    pending INTEGER NOT NULL,
    lost INTEGER NOT NULL,
    invalid INTEGER NOT NULL,
    queue_full INTEGER NOT NULL,
    closed INTEGER NOT NULL,
    store_failures INTEGER NOT NULL
);

CREATE INDEX llm_saved_trace_capture_sessions_window_idx
    ON llm_saved_trace_capture_sessions (started_at_unix_ns, updated_at_unix_ns);
