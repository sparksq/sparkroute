CREATE TABLE IF NOT EXISTS llm_config_revisions (
    revision_id TEXT PRIMARY KEY,
    document_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL,
    reason TEXT,
    CHECK (length(revision_id) = 64),
    CHECK (jsonb_typeof(document_json) = 'object'),
    CHECK (length(created_by) BETWEEN 1 AND 512),
    CHECK (reason IS NULL OR length(reason) <= 4096)
);

CREATE OR REPLACE FUNCTION reject_llm_config_revision_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'llm_config_revisions rows are immutable'
        USING ERRCODE = '55000';
END;
$$;

DROP TRIGGER IF EXISTS llm_config_revisions_immutable
    ON llm_config_revisions;
CREATE TRIGGER llm_config_revisions_immutable
    BEFORE UPDATE OR DELETE ON llm_config_revisions
    FOR EACH ROW
    EXECUTE FUNCTION reject_llm_config_revision_mutation();

CREATE TABLE IF NOT EXISTS llm_config_state (
    singleton SMALLINT PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
    active_revision_id TEXT NOT NULL
        REFERENCES llm_config_revisions (revision_id),
    generation BIGINT NOT NULL CHECK (generation > 0),
    activated_at TIMESTAMPTZ NOT NULL,
    activated_by TEXT NOT NULL,
    reason TEXT,
    CHECK (length(activated_by) BETWEEN 1 AND 512),
    CHECK (reason IS NULL OR length(reason) <= 4096)
);

CREATE TABLE IF NOT EXISTS llm_config_activations (
    generation BIGINT PRIMARY KEY,
    revision_id TEXT NOT NULL
        REFERENCES llm_config_revisions (revision_id),
    previous_revision_id TEXT
        REFERENCES llm_config_revisions (revision_id),
    activated_at TIMESTAMPTZ NOT NULL,
    activated_by TEXT NOT NULL,
    reason TEXT,
    CHECK (generation > 0),
    CHECK (length(activated_by) BETWEEN 1 AND 512),
    CHECK (reason IS NULL OR length(reason) <= 4096)
);

CREATE INDEX IF NOT EXISTS llm_config_revisions_created_idx
    ON llm_config_revisions (created_at DESC, revision_id DESC);
CREATE INDEX IF NOT EXISTS llm_config_activations_revision_idx
    ON llm_config_activations (revision_id, generation DESC);
