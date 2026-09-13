ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS principal_id TEXT;
ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS principal_type TEXT;
ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS principal_subject TEXT;
ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS principal_roles_json JSONB;
ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE llm_requests
    ADD COLUMN IF NOT EXISTS attribution_json JSONB;

CREATE INDEX IF NOT EXISTS llm_requests_tenant_seek_idx
    ON llm_requests (tenant_id, started_at DESC, request_id DESC);
