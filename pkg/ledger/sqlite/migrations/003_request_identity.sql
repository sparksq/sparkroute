ALTER TABLE llm_requests
    ADD COLUMN principal_id TEXT;
ALTER TABLE llm_requests
    ADD COLUMN principal_type TEXT;
ALTER TABLE llm_requests
    ADD COLUMN principal_subject TEXT;
ALTER TABLE llm_requests
    ADD COLUMN principal_roles_json TEXT;
ALTER TABLE llm_requests
    ADD COLUMN tenant_id TEXT;
ALTER TABLE llm_requests
    ADD COLUMN attribution_json TEXT;

CREATE INDEX IF NOT EXISTS llm_requests_tenant_seek_idx
    ON llm_requests (tenant_id, started_at_unix_ns DESC, request_id DESC);
