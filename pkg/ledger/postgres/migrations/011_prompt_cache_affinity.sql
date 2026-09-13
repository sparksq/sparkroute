CREATE TABLE IF NOT EXISTS llm_prompt_cache_affinities (
    scope_digest TEXT NOT NULL,
    virtual_model TEXT NOT NULL,
    operation TEXT NOT NULL,
    config_revision TEXT NOT NULL,
    prefix_digest TEXT NOT NULL,
    prefix_bytes BIGINT NOT NULL,
    prefix_segments INTEGER NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    upstream_protocol TEXT NOT NULL,
    last_success_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        scope_digest,
        virtual_model,
        operation,
        config_revision,
        prefix_digest,
        deployment
    )
);

CREATE INDEX IF NOT EXISTS llm_prompt_cache_affinities_expires_idx
    ON llm_prompt_cache_affinities (expires_at);

CREATE INDEX IF NOT EXISTS llm_prompt_cache_affinities_lookup_idx
    ON llm_prompt_cache_affinities (
        scope_digest,
        virtual_model,
        operation,
        config_revision,
        prefix_digest,
        last_success_at DESC
    );
