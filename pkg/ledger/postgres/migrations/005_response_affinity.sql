CREATE TABLE IF NOT EXISTS llm_response_affinities (
	owner_scope TEXT NOT NULL
		CHECK (octet_length(owner_scope) BETWEEN 1 AND 128),
    response_id TEXT NOT NULL
		CHECK (octet_length(response_id) BETWEEN 1 AND 512),
    virtual_model TEXT NOT NULL,
    provider TEXT NOT NULL,
    deployment TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    bound_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
	CHECK (expires_at > bound_at),
	PRIMARY KEY (owner_scope, response_id)
);

CREATE INDEX IF NOT EXISTS llm_response_affinities_expiry_idx
    ON llm_response_affinities (expires_at);
